package k8s

import (
	"sync"
	"time"
)

// InstanceState 函数实例状态
type InstanceState string

const (
	InstanceStateIdle InstanceState = "idle" // 空闲
	InstanceStateBusy InstanceState = "busy" // 忙碌
	InstanceStateDown InstanceState = "down" // 不可用
)

// FunctionInstance 函数实例元信息+状态
type FunctionInstance struct {
	FunctionName string        // 函数名
	Namespace    string        // 命名空间
	PodIP        string        // Pod IP（对应Endpoint的Address.IP）
	State        InstanceState // 实例状态
	UpdateTime   time.Time     // 状态最后更新时间
}

// InstanceStatusManager 实例状态管理器（单例/全局）
type InstanceStatusManager struct {
	lock      sync.RWMutex
	instances map[string]*FunctionInstance // key: <functionName>.<namespace>.<podIP>
}

// NewInstanceStatusManager 初始化管理器
func NewInstanceStatusManager() *InstanceStatusManager {
	return &InstanceStatusManager{
		instances: make(map[string]*FunctionInstance),
	}
}

// GetInstanceKey 生成实例唯一标识
func (m *InstanceStatusManager) GetInstanceKey(funcName, ns, podIP string) string {
	return funcName + "." + ns + "." + podIP
}

// AddInstance 新增/初始化实例（Pod创建时调用）
func (m *InstanceStatusManager) AddInstance(funcName, ns, podIP string) {
	m.lock.Lock()
	defer m.lock.Unlock()
	key := m.GetInstanceKey(funcName, ns, podIP)
	m.instances[key] = &FunctionInstance{
		FunctionName: funcName,
		Namespace:    ns,
		PodIP:        podIP,
		State:        InstanceStateIdle,
		UpdateTime:   time.Now(),
	}
}

// RemoveInstance 删除实例（Pod删除时调用）
func (m *InstanceStatusManager) RemoveInstance(funcName, ns, podIP string) {
	m.lock.Lock()
	defer m.lock.Unlock()
	delete(m.instances, m.GetInstanceKey(funcName, ns, podIP))
}

// UpdateInstanceState 更新实例状态
func (m *InstanceStatusManager) UpdateInstanceState(funcName, ns, podIP string, state InstanceState) {
	m.lock.Lock()
	defer m.lock.Unlock()
	key := m.GetInstanceKey(funcName, ns, podIP)
	if inst, ok := m.instances[key]; ok {
		inst.State = state
		inst.UpdateTime = time.Now()
	}
}

// ListIdleInstances 筛选指定函数的空闲实例
func (m *InstanceStatusManager) ListIdleInstances(funcName, ns string) []*FunctionInstance {
	m.lock.RLock()
	defer m.lock.RUnlock()
	var idle []*FunctionInstance
	for _, inst := range m.instances {
		if inst.FunctionName == funcName && inst.Namespace == ns && inst.State == InstanceStateIdle {
			idle = append(idle, inst)
		}
	}
	return idle
}

// ListAllInstances 列出指定函数的所有实例（降级用）
func (m *InstanceStatusManager) ListAllInstances(funcName, ns string) []*FunctionInstance {
	m.lock.RLock()
	defer m.lock.RUnlock()
	var all []*FunctionInstance
	for _, inst := range m.instances {
		if inst.FunctionName == funcName && inst.Namespace == ns && inst.State != InstanceStateDown {
			all = append(all, inst)
		}
	}
	return all
}
