// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controlapi

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	listersv1alpha1 "github.com/agent-substrate/substrate/pkg/client/listers/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// WorkerPoolSyncer reconciles the state of worker pods from Kubernetes Informer
// into the store.
type WorkerPoolSyncer struct {
	persistence      store.Interface
	workerInformer   cache.SharedIndexInformer
	workerPoolLister listersv1alpha1.WorkerPoolLister

	// restartMu guards restartCounts, which tracks the last observed container
	// restart count per worker pod UID. A worker pod can restart its container
	// in place (OOM/crash): the pod object survives, so the DeleteFunc never
	// runs and the actor bound to that worker would otherwise stay
	// STATUS_RUNNING on a worker whose sandbox is gone. The counts are
	// in-memory on purpose: a freshly started ateapi has no history and must
	// not mass-release actors; it only records the current counts.
	restartMu     sync.Mutex
	restartCounts map[string]int32
}

// NewWorkerPoolSyncer creates a new WorkerPoolSyncer.
func NewWorkerPoolSyncer(persistence store.Interface, workerInformer cache.SharedIndexInformer, workerPoolLister listersv1alpha1.WorkerPoolLister) *WorkerPoolSyncer {
	return &WorkerPoolSyncer{
		persistence:      persistence,
		workerInformer:   workerInformer,
		workerPoolLister: workerPoolLister,
		restartCounts:    make(map[string]int32),
	}
}

// Start starts the background reconciliation loop.
func (s *WorkerPoolSyncer) Start(ctx context.Context) {
	s.workerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			s.observeRestart(pod)
			s.syncWorkerToStore(ctx, pod)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod := newObj.(*corev1.Pod)
			if s.observeRestart(pod) {
				s.handleWorkerRestart(ctx, pod)
				return
			}
			s.syncWorkerToStore(ctx, pod)
		},
		DeleteFunc: func(obj interface{}) {
			var pod *corev1.Pod
			switch t := obj.(type) {
			case *corev1.Pod:
				pod = t
			case cache.DeletedFinalStateUnknown:
				var ok bool
				pod, ok = t.Obj.(*corev1.Pod)
				if !ok {
					slog.ErrorContext(ctx, "Failed to cast DeletedFinalStateUnknown object to Pod")
					return
				}
			default:
				slog.ErrorContext(ctx, "Unknown object type in delete handler", slog.Any("obj", obj))
				return
			}
			s.forgetRestart(pod)
			slog.InfoContext(ctx, "Syncer: removing worker from store", slog.String("worker", pod.Namespace+"/"+pod.Name))
			if err := s.releaseActorOnDeadWorker(ctx, pod.Namespace, pod.Labels[workerPodLabel], pod.Name); err != nil {
				slog.ErrorContext(ctx, "Failed to release actor bound to deleted worker", slog.Any("err", err))
			}
			err := s.persistence.DeleteWorker(ctx, pod.Namespace, pod.Labels[workerPodLabel], pod.Name)
			if err != nil {
				slog.ErrorContext(ctx, "Failed to delete worker from store during delete event", slog.Any("err", err))
			}
		},
	})

	go func() {
		if !cache.WaitForCacheSync(ctx.Done(), s.workerInformer.HasSynced) {
			slog.ErrorContext(ctx, "Syncer: failed to sync informer cache")
			return
		}

		slog.InfoContext(ctx, "Syncer: performing initial sync on startup")
		objs := s.workerInformer.GetIndexer().List()
		for _, obj := range objs {
			pod := obj.(*corev1.Pod)
			s.observeRestart(pod)
			s.syncWorkerToStore(ctx, pod)
		}
	}()
}

func (s *WorkerPoolSyncer) syncWorkerToStore(ctx context.Context, pod *corev1.Pod) {
	if !isWorkerEligible(pod) {
		return
	}

	if pod.DeletionTimestamp != nil {
		slog.InfoContext(ctx, "Syncer: removing worker from store (pod deleting)", slog.String("worker", pod.Namespace+"/"+pod.Name))
		if err := s.releaseActorOnDeadWorker(ctx, pod.Namespace, pod.Labels[workerPodLabel], pod.Name); err != nil {
			slog.ErrorContext(ctx, "Failed to release actor bound to soft-deleting worker", slog.Any("err", err))
		}
		err := s.persistence.DeleteWorker(ctx, pod.Namespace, pod.Labels[workerPodLabel], pod.Name)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to delete worker from store during update event (deleting)", slog.Any("err", err))
		}
		return
	}

	poolName := pod.Labels[workerPodLabel]
	pool, err := s.workerPoolLister.WorkerPools(pod.Namespace).Get(poolName)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get WorkerPool for worker pod", slog.String("worker", pod.Namespace+"/"+pod.Name), slog.String("pool", poolName), slog.Any("err", err))
		return
	}

	w, err := s.persistence.GetWorker(ctx, pod.Namespace, poolName, pod.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.InfoContext(ctx, "Syncer: creating worker in store", slog.String("worker", pod.Namespace+"/"+pod.Name))
			worker := &ateapipb.Worker{
				WorkerNamespace: pod.Namespace,
				WorkerPool:      poolName,
				WorkerPod:       pod.Name,
				Ip:              pod.Status.PodIP,
				WorkerPodUid:    string(pod.UID),
				NodeName:        pod.Spec.NodeName,
				SandboxClass:    string(pool.Spec.SandboxClass),
				Labels:          pool.GetLabels(),
			}
			// TODO(thockin): for now this is the only place Workers are
			// created.  If/when this becomes a regular API, validation should
			// move there.
			if errs := resources.ValidateWorker(worker, nil); len(errs) > 0 {
				err := status.Error(codes.InvalidArgument, errs.ToAggregate().Error())
				slog.ErrorContext(ctx, "Invalid worker", slog.Any("err", err))
				return
			}
			err = s.persistence.CreateWorker(ctx, worker)
			if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
				slog.ErrorContext(ctx, "Failed to create worker in store", slog.Any("err", err))
			}
			return
		}
		slog.ErrorContext(ctx, "Failed to get worker from store", slog.Any("err", err))
		return
	}

	changed := false
	if w.Ip != pod.Status.PodIP {
		// TODO: I don't think this is possible, but handling this case so we can log it just in case we can reproduce it.
		slog.InfoContext(ctx, "Syncer: updating worker in store (IP changed)", slog.String("worker", pod.Namespace+"/"+pod.Name))
		w.Ip = pod.Status.PodIP
		changed = true
	}
	if w.SandboxClass != string(pool.Spec.SandboxClass) {
		slog.InfoContext(ctx, "Syncer: updating worker in store (SandboxClass changed)", slog.String("worker", pod.Namespace+"/"+pod.Name))
		w.SandboxClass = string(pool.Spec.SandboxClass)
		changed = true
	}
	if !maps.Equal(w.Labels, pool.GetLabels()) {
		slog.InfoContext(ctx, "Syncer: updating worker in store (labels changed)", slog.String("worker", pod.Namespace+"/"+pod.Name))
		w.Labels = pool.GetLabels()
		changed = true
	}

	if changed {
		if err = s.persistence.UpdateWorker(ctx, w, w.Version); err != nil {
			slog.ErrorContext(ctx, "Failed to update worker in store", slog.Any("err", err))
		}
	}
}

// workerRestartCount returns the largest container restart count in the pod.
func workerRestartCount(pod *corev1.Pod) int32 {
	var max int32
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.RestartCount > max {
			max = cs.RestartCount
		}
	}
	return max
}

// observeRestart records the pod's restart count and reports whether it grew
// since the previous observation. The first observation only records.
func (s *WorkerPoolSyncer) observeRestart(pod *corev1.Pod) bool {
	uid := string(pod.UID)
	if uid == "" {
		return false
	}
	rc := workerRestartCount(pod)
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	prev, seen := s.restartCounts[uid]
	s.restartCounts[uid] = rc
	return seen && rc > prev
}

func (s *WorkerPoolSyncer) forgetRestart(pod *corev1.Pod) {
	if uid := string(pod.UID); uid != "" {
		s.restartMu.Lock()
		delete(s.restartCounts, uid)
		s.restartMu.Unlock()
	}
}

// handleWorkerRestart reacts to a container restart inside a surviving worker
// pod: the sandbox is gone, so release the actor (SUSPENDED, with its data on
// the sticky volume) and re-materialise the worker as free. The next request
// resumes the actor with a cold boot.
func (s *WorkerPoolSyncer) handleWorkerRestart(ctx context.Context, pod *corev1.Pod) {
	poolName := pod.Labels[workerPodLabel]
	slog.InfoContext(ctx, "Syncer: worker container restarted; releasing its actor",
		slog.String("worker", pod.Namespace+"/"+pod.Name),
		slog.Int("restartCount", int(workerRestartCount(pod))))
	if err := s.releaseActorOnDeadWorker(ctx, pod.Namespace, poolName, pod.Name); err != nil {
		slog.ErrorContext(ctx, "Failed to release actor bound to restarted worker", slog.Any("err", err))
	}
	if err := s.persistence.DeleteWorker(ctx, pod.Namespace, poolName, pod.Name); err != nil {
		slog.ErrorContext(ctx, "Failed to delete restarted worker from store", slog.Any("err", err))
	}
	// Re-add the blank worker as free. If it has no IP yet, a later update does.
	s.syncWorkerToStore(ctx, pod)
}

func isWorkerEligible(pod *corev1.Pod) bool {
	return pod.Status.PodIP != ""
}

// releaseActorOnDeadWorker resets the actor bound to a vanishing worker
// pod back to STATUS_SUSPENDED so the next request reassigns it.
//
// UpdateActor uses optimistic version checking. A concurrent SuspendActor
// or ResumeActor wins; we drop this attempt silently.
//
// Best-effort only. The caller always proceeds to DeleteWorker after this
// returns, so any non-contention failure leaves the actor stranded
// (STATUS_RUNNING, pointer at a pod that no longer exists). Recovery
// then needs a manual SuspendActor.
//
// The long-term fix is a finalizer-based controller that holds the pod
// in Terminating state until the actor is gracefully suspended. Tracked
// in https://github.com/agent-substrate/substrate/issues/23.
func (s *WorkerPoolSyncer) releaseActorOnDeadWorker(ctx context.Context, namespace, pool, podName string) error {
	worker, err := s.persistence.GetWorker(ctx, namespace, pool, podName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if worker.Assignment == nil {
		return nil
	}
	actor, err := s.persistence.GetActor(ctx, worker.Assignment.Actor.Atespace, worker.Assignment.Actor.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	// Skip if a concurrent SuspendActor already cleared the pointer.
	if actor.GetAteomPodNamespace() != namespace || actor.GetAteomPodName() != podName {
		return nil
	}
	if actor.Status != ateapipb.Actor_STATUS_CRASHED {
		actor.Status = ateapipb.Actor_STATUS_SUSPENDED
	}
	actor.AteomPodNamespace = ""
	actor.AteomPodName = ""
	actor.AteomPodIp = ""
	actor.InProgressSnapshot = ""
	actor.WorkerPoolName = ""
	if _, err := s.persistence.UpdateActor(ctx, actor, actor.GetMetadata().GetVersion()); err != nil && !errors.Is(err, store.ErrVersionConflict) {
		return err
	}
	return nil
}
