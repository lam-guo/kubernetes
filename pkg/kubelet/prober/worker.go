/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package prober

import (
	"math/rand"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
)

// worker handles the periodic probing of its assigned container. Each worker has a go-routine
// associated with it which runs the probe loop until the container permanently terminates, or the
// stop channel is closed. The worker uses the probe Manager's statusManager to get up-to-date
// container IDs.

// worker处理他分配到的容器的周期性勘探。
// 每个worker有一个【直到容器永久终止或stop channel关闭之前】都在执行勘探循环的go-routine。
// worker使用probe manager的statusManager去获取最新的容器IDs
type worker struct {
	// Channel for stopping the probe.
	stopCh chan struct{}

	// Channel for triggering the probe manually.
	manualTriggerCh chan struct{}

	// The pod containing this probe (read-only)
	pod *v1.Pod

	// The container to probe (read-only)
	container v1.Container

	// Describes the probe configuration (read-only)
	spec *v1.Probe

	// The type of the worker.
	probeType probeType

	// The probe value during the initial delay.
	initialValue results.Result

	// Where to store this workers results.
	resultsManager results.Manager
	probeManager   *manager

	// The last known container ID for this worker.
	containerID kubecontainer.ContainerID
	// The last probe result for this worker.
	lastResult results.Result
	// How many times in a row the probe has returned the same result.
	resultRun int

	// If set, skip probing.
	onHold bool

	// proberResultsMetricLabels holds the labels attached to this worker
	// for the ProberResults metric by result.
	proberResultsSuccessfulMetricLabels metrics.Labels
	proberResultsFailedMetricLabels     metrics.Labels
	proberResultsUnknownMetricLabels    metrics.Labels
}

// Creates and starts a new probe worker.
func newWorker(
	m *manager,
	probeType probeType,
	pod *v1.Pod,
	container v1.Container) *worker {

	w := &worker{
		stopCh:          make(chan struct{}, 1), // Buffer so stop() can be non-blocking.
		manualTriggerCh: make(chan struct{}, 1), // Buffer so prober_manager can do non-blocking calls to doProbe.
		pod:             pod,
		container:       container,
		probeType:       probeType,
		probeManager:    m,
	}

	switch probeType {
	case readiness:
		w.spec = container.ReadinessProbe
		w.resultsManager = m.readinessManager
		w.initialValue = results.Failure
	case liveness:
		w.spec = container.LivenessProbe
		w.resultsManager = m.livenessManager
		w.initialValue = results.Success
	case startup:
		w.spec = container.StartupProbe
		w.resultsManager = m.startupManager
		w.initialValue = results.Unknown
	}

	basicMetricLabels := metrics.Labels{
		"probe_type": w.probeType.String(),
		"container":  w.container.Name,
		"pod":        w.pod.Name,
		"namespace":  w.pod.Namespace,
		"pod_uid":    string(w.pod.UID),
	}

	w.proberResultsSuccessfulMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsSuccessfulMetricLabels["result"] = probeResultSuccessful

	w.proberResultsFailedMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsFailedMetricLabels["result"] = probeResultFailed

	w.proberResultsUnknownMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsUnknownMetricLabels["result"] = probeResultUnknown

	return w
}

// run periodically probes the container.
func (w *worker) run() {
	probeTickerPeriod := time.Duration(w.spec.PeriodSeconds) * time.Second

	// If kubelet restarted the probes could be started in rapid succession.
	// Let the worker wait for a random portion of tickerPeriod before probing.
	// Do it only if the kubelet has started recently.
	if probeTickerPeriod > time.Since(w.probeManager.start) {
		time.Sleep(time.Duration(rand.Float64() * float64(probeTickerPeriod)))
	}

	probeTicker := time.NewTicker(probeTickerPeriod)

	defer func() {
		// Clean up.
		probeTicker.Stop()
		if !w.containerID.IsEmpty() {
			w.resultsManager.Remove(w.containerID)
		}

		w.probeManager.removeWorker(w.pod.UID, w.container.Name, w.probeType)
		ProberResults.Delete(w.proberResultsSuccessfulMetricLabels)
		ProberResults.Delete(w.proberResultsFailedMetricLabels)
		ProberResults.Delete(w.proberResultsUnknownMetricLabels)
	}()

	// label用法，跳出for w.doProbe循环
probeLoop:
	for w.doProbe() {
		// Wait for next probe tick.
		// 等待下次probe
		// 如果stopCh有消息，断开probeLoop
		select {
		case <-w.stopCh:
			break probeLoop
		case <-probeTicker.C:
		case <-w.manualTriggerCh:
			// continue
		}
	}
}

// stop stops the probe worker. The worker handles cleanup and removes itself from its manager.
// It is safe to call stop multiple times.
// stop() 停止probe worker。
// worker处理清理和从他的manager中移除自己。
// 可以安全地调用多次
// 会停止probe loop
func (w *worker) stop() {
	select {
	case w.stopCh <- struct{}{}:
	default: // Non-blocking.
	}
}

// doProbe probes the container once and records the result.
// Returns whether the worker should continue.
// 勘察一遍容器并且记录结果
// 返回这个worker是否应该继续
func (w *worker) doProbe() (keepGoing bool) {
	defer func() { recover() }() // Actually eat panics (HandleCrash takes care of logging)
	defer runtime.HandleCrash(func(_ interface{}) { keepGoing = true })

	status, ok := w.probeManager.statusManager.GetPodStatus(w.pod.UID)
	if !ok {
		// Either the pod has not been created yet, or it was already deleted.
		klog.V(3).InfoS("No status for pod", "pod", klog.KObj(w.pod))
		return true
	}

	// Worker should terminate if pod is terminated.
	// pod终止了worker也应该终止
	if status.Phase == v1.PodFailed || status.Phase == v1.PodSucceeded {
		klog.V(3).InfoS("Pod is terminated, exiting probe worker",
			"pod", klog.KObj(w.pod), "phase", status.Phase)
		return false
	}

	// 通过worker容器名获取容器信息
	c, ok := podutil.GetContainerStatus(status.ContainerStatuses, w.container.Name)
	// 如果容器没创建或者被删除，返回true,等下一次信息
	if !ok || len(c.ContainerID) == 0 {
		// Either the container has not been created yet, or it was deleted.
		klog.V(3).InfoS("Probe target container not found",
			"pod", klog.KObj(w.pod), "containerName", w.container.Name)
		return true // Wait for more information.
	}

	// 如果woker的容器id不等于通过GetContainerStatus获取容器id，worker的容器id改为GetContainerStatus获取的容器id
	// 记录一条容器初始化信息
	// 恢复（继续）勘察
	if w.containerID.String() != c.ContainerID {
		if !w.containerID.IsEmpty() {
			w.resultsManager.Remove(w.containerID)
		}
		w.containerID = kubecontainer.ParseContainerID(c.ContainerID)
		w.resultsManager.Set(w.containerID, w.initialValue, w.pod)
		// We've got a new container; resume probing.
		w.onHold = false
	}

	// worker一直等待，直到有新的容器
	if w.onHold {
		// Worker is on hold until there is a new container.
		return true
	}

	// 如果没有勘探到正在运行的容器
	// 如果容器不会被重启就中断了，判断依据（容器被终止 或 RestartPolicy不为RestartPolicyNever）
	if c.State.Running == nil {
		klog.V(3).InfoS("Non-running container probed",
			"pod", klog.KObj(w.pod), "containerName", w.container.Name)
		if !w.containerID.IsEmpty() {
			w.resultsManager.Set(w.containerID, results.Failure, w.pod)
		}
		// Abort if the container will not be restarted.
		return c.State.Terminated == nil ||
			w.pod.Spec.RestartPolicy != v1.RestartPolicyNever
	}

	// Graceful shutdown of the pod.
	// 优雅关闭pod
	// 如果有DeletionTimestamp（资源关闭日期）且probeType为liveness或startup
	// 记录勘探结果为success
	// 停止勘探
	if w.pod.ObjectMeta.DeletionTimestamp != nil && (w.probeType == liveness || w.probeType == startup) {
		klog.V(3).InfoS("Pod deletion requested, setting probe result to success",
			"probeType", w.probeType, "pod", klog.KObj(w.pod), "containerName", w.container.Name)
		if w.probeType == startup {
			klog.InfoS("Pod deletion requested before container has fully started",
				"pod", klog.KObj(w.pod), "containerName", w.container.Name)
		}
		// Set a last result to ensure quiet shutdown.
		w.resultsManager.Set(w.containerID, results.Success, w.pod)
		// Stop probing at this point.
		return false
	}

	// 启动时间小于InitialDelaySeconds（初始化延迟时间）先跳过这次勘探
	// Probe disabled for InitialDelaySeconds.
	if int32(time.Since(c.State.Running.StartedAt.Time).Seconds()) < w.spec.InitialDelaySeconds {
		return true
	}

	// TODO: 还没看懂
	if c.Started != nil && *c.Started {
		// Stop probing for startup once container has started.
		// we keep it running to make sure it will work for restarted container.
		if w.probeType == startup {
			return true
		}
	} else {
		// Disable other probes until container has started.
		if w.probeType != startup {
			return true
		}
	}

	// TODO: in order for exec probes to correctly handle downward API env, we must be able to reconstruct
	// the full container environment here, OR we must make a call to the CRI in order to get those environment
	// values from the running container.
	// 为了执行勘 探获 得正确地处理downward API env（啥玩意？）。
	// 我们必须在这重新组合好完全的容器环境信息,或者我们必须调用CRI去 获取正在运行的容器的环境信息
	result, err := w.probeManager.prober.probe(w.probeType, w.pod, status, w.container, w.containerID)
	if err != nil {
		// Prober error, throw away the result.
		return true
	}

	// 性能指标计数器增加相应类型计数
	switch result {
	case results.Success:
		ProberResults.With(w.proberResultsSuccessfulMetricLabels).Inc()
	case results.Failure:
		ProberResults.With(w.proberResultsFailedMetricLabels).Inc()
	default:
		ProberResults.With(w.proberResultsUnknownMetricLabels).Inc()
	}

	if w.lastResult == result {
		w.resultRun++
	} else {
		w.lastResult = result
		w.resultRun = 1
	}

	// 如果成功或失败次数小于阈值，不管了，状态不变
	if (result == results.Failure && w.resultRun < int(w.spec.FailureThreshold)) ||
		(result == results.Success && w.resultRun < int(w.spec.SuccessThreshold)) {
		// Success or failure is below threshold - leave the probe state unchanged.
		return true
	}

	// 保存状态
	w.resultsManager.Set(w.containerID, result, w.pod)

	if (w.probeType == liveness || w.probeType == startup) && result == results.Failure {
		// The container fails a liveness/startup check, it will need to be restarted.
		// Stop probing until we see a new container ID. This is to reduce the
		// chance of hitting #21751, where running `docker exec` when a
		// container is being stopped may lead to corrupted container state.
		w.onHold = true
		w.resultRun = 0
	}

	return true
}

func deepCopyPrometheusLabels(m metrics.Labels) metrics.Labels {
	ret := make(metrics.Labels, len(m))
	for k, v := range m {
		ret[k] = v
	}
	return ret
}
