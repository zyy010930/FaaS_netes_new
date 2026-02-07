// Copyright (c) Alex Ellis 2017. All rights reserved.
// Copyright 2020 OpenFaaS Author(s)
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package k8s

import (
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"strings"
	"sync"

	corelister "k8s.io/client-go/listers/core/v1"
)

// watchdogPort for the OpenFaaS function watchdog
const watchdogPort = 8080

//func NewFunctionLookup(ns string, lister corelister.EndpointsLister) *FunctionLookup {
//	return &FunctionLookup{
//		DefaultNamespace: ns,
//		EndpointLister:   lister,
//		Listers:          map[string]corelister.EndpointsNamespaceLister{},
//		lock:             sync.RWMutex{},
//	}
//}
//
//type FunctionLookup struct {
//	DefaultNamespace string
//	EndpointLister   corelister.EndpointsLister
//	Listers          map[string]corelister.EndpointsNamespaceLister
//
//	lock sync.RWMutex
//}

func (f *FunctionLookup) GetLister(ns string) corelister.EndpointsNamespaceLister {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return f.Listers[ns]
}

func (f *FunctionLookup) SetLister(ns string, lister corelister.EndpointsNamespaceLister) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.Listers[ns] = lister
}

func getNamespace(name, defaultNamespace string) string {
	namespace := defaultNamespace
	if strings.Contains(name, ".") {
		namespace = name[strings.LastIndexAny(name, ".")+1:]
	}
	return namespace
}

// 注入状态管理器（需修改NewFunctionLookup，添加实例管理器参数）
type FunctionLookup struct {
	DefaultNamespace string
	EndpointLister   corelister.EndpointsLister
	Listers          map[string]corelister.EndpointsNamespaceLister
	lock             sync.RWMutex
	instanceManager  *InstanceStatusManager // 新增：状态管理器
}

// 重构NewFunctionLookup，传入状态管理器
func NewFunctionLookup(ns string, lister corelister.EndpointsLister, instMgr *InstanceStatusManager) *FunctionLookup {
	return &FunctionLookup{
		DefaultNamespace: ns,
		EndpointLister:   lister,
		Listers:          map[string]corelister.EndpointsNamespaceLister{},
		lock:             sync.RWMutex{},
		instanceManager:  instMgr,
	}
}

// 改造Resolve方法：优先选空闲实例
func (l *FunctionLookup) Resolve(name string) (url.URL, error) {
	functionName := name
	namespace := getNamespace(name, l.DefaultNamespace)
	if err := l.verifyNamespace(namespace); err != nil {
		return url.URL{}, err
	}

	if strings.Contains(name, ".") {
		functionName = strings.TrimSuffix(name, "."+namespace)
	}

	nsEndpointLister := l.GetLister(namespace)
	if nsEndpointLister == nil {
		l.SetLister(namespace, l.EndpointLister.Endpoints(namespace))
		nsEndpointLister = l.GetLister(namespace)
	}

	// 1. 查询Endpoint获取所有可用实例IP
	svc, err := nsEndpointLister.Get(functionName)
	if err != nil {
		return url.URL{}, fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
	}
	if len(svc.Subsets) == 0 || len(svc.Subsets[0].Addresses) == 0 {
		return url.URL{}, fmt.Errorf("no addresses in subset for \"%s.%s\"", functionName, namespace)
	}

	// 2. 从状态管理器筛选空闲实例
	idleInstances := l.instanceManager.ListIdleInstances(functionName, namespace)

	// 2.0 无空闲实例：添加所有实例
	if len(idleInstances) == 0 {
		log.Printf("No idle instances found for function %s", functionName)
		for i := 0; i < len(svc.Subsets[0].Addresses); i++ {
			ip := svc.Subsets[0].Addresses[i].IP
			l.instanceManager.AddInstance(functionName, namespace, ip)
		}
	}
	var targetIP string
	if len(idleInstances) > 0 {
		// 2.1 有空闲实例：随机选一个
		targetIdx := rand.Intn(len(idleInstances))
		targetIP = idleInstances[targetIdx].PodIP
		log.Printf("Selected idle instance %s for function %s, len = %d", targetIP, functionName, len(idleInstances))
		// 标记为忙碌（需在请求完成后重置，见步骤4）
		l.instanceManager.UpdateInstanceState(functionName, namespace, targetIP, InstanceStateBusy)
	} else {
		// 2.2 无空闲实例：降级为随机选（或返回错误/等待）
		allAddrs := svc.Subsets[0].Addresses
		targetIdx := rand.Intn(len(allAddrs))
		targetIP = allAddrs[targetIdx].IP
	}

	// 3. 生成访问URL
	port := svc.Subsets[0].Ports[0].Port
	urlStr := fmt.Sprintf("http://%s:%d", targetIP, port)
	urlRes, err := url.Parse(urlStr)
	if err != nil {
		return url.URL{}, err
	}

	log.Printf("Resolved function %s to %s", functionName, urlRes)
	return *urlRes, nil
}

//func (l *FunctionLookup) Resolve(name string) (url.URL, error) {
//	functionName := name
//	namespace := getNamespace(name, l.DefaultNamespace)
//	if err := l.verifyNamespace(namespace); err != nil {
//		return url.URL{}, err
//	}
//
//	if strings.Contains(name, ".") {
//		functionName = strings.TrimSuffix(name, "."+namespace)
//	}
//
//	nsEndpointLister := l.GetLister(namespace)
//
//	if nsEndpointLister == nil {
//		l.SetLister(namespace, l.EndpointLister.Endpoints(namespace))
//
//		nsEndpointLister = l.GetLister(namespace)
//	}
//
//	svc, err := nsEndpointLister.Get(functionName)
//	log.Printf("function = %s, svc = %s", functionName, svc)
//	if err != nil {
//		return url.URL{}, fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
//	}
//
//	if len(svc.Subsets) == 0 {
//		return url.URL{}, fmt.Errorf("no subsets available for \"%s.%s\"", functionName, namespace)
//	}
//
//	all := len(svc.Subsets[0].Addresses)
//	if len(svc.Subsets[0].Addresses) == 0 {
//		return url.URL{}, fmt.Errorf("no addresses in subset for \"%s.%s\"", functionName, namespace)
//	}
//
//	target := rand.Intn(all)
//
//	serviceIP := svc.Subsets[0].Addresses[target].IP
//	//port := svc.Subsets[0].Ports[target].Port
//	port := svc.Subsets[0].Ports[0].Port
//
//	// urlStr := fmt.Sprintf("http://%s:%d", serviceIP, watchdogPort)
//	urlStr := fmt.Sprintf("http://%s:%d", serviceIP, port)
//
//	urlRes, err := url.Parse(urlStr)
//	if err != nil {
//		return url.URL{}, err
//	}
//	log.Printf("Resolved function %s to %s", functionName, urlRes)
//	return *urlRes, nil
//}

func (l *FunctionLookup) verifyNamespace(name string) error {
	if name != "kube-system" {
		return nil
	}
	// ToDo use global namepace parse and validation
	return fmt.Errorf("namespace not allowed")
}
