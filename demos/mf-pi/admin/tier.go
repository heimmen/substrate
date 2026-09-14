// Per-user resource-tier plumbing for the mf-pi admin server.
//
// Each mf-pi user is one Actor in the mfpi atespace. The admin can assign a
// resource tier (e.g. small/mid/large); each tier maps to a dedicated
// ActorTemplate + WorkerPool (pre-created at deploy time) so the actor's CPU /
// memory / disk are actually constrained. The stored tier takes effect on the
// next fresh create/recreate of the user's actor (existing actors keep their
// template). Mirrors the persistence style of configMapExpiryStore:
// pre-created ConfigMap + namespace-scoped RBAC with only get/update.
package main

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// tierStore persists per-user resource tiers. Implemented by
// configMapTierStore in production and by a fake in tests.
type tierStore interface {
	// Get returns the stored tier for the user, and whether one exists.
	Get(name string) (tier string, ok bool)
	// Set stores (overwrites) the tier for the user.
	Set(name, tier string) error
	// Delete removes any stored tier for the user (idempotent).
	Delete(name string) error
}

// configMapTierStore stores per-user resource tiers in a Kubernetes ConfigMap
// (username -> tier), mirroring them into an in-memory map.
type configMapTierStore struct {
	clientset kubernetes.Interface
	namespace string
	name      string

	mu   sync.RWMutex
	data map[string]string // username -> tier
}

func newConfigMapTierStore(clientset kubernetes.Interface, namespace, name string) *configMapTierStore {
	return &configMapTierStore{
		clientset: clientset,
		namespace: namespace,
		name:      name,
		data:      map[string]string{},
	}
}

// load populates the in-memory map from the ConfigMap. A missing ConfigMap is
// treated as an empty store (not an error).
func (s *configMapTierStore) load(ctx context.Context) error {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string]string, len(cm.Data))
	for k, v := range cm.Data {
		s.data[k] = v
	}
	return nil
}

func (s *configMapTierStore) Get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[name]
	return v, ok
}

func (s *configMapTierStore) Set(name, tier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string, len(s.data)+1)
	for k, v := range s.data {
		next[k] = v
	}
	next[name] = tier
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *configMapTierStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[name]; !ok {
		return nil
	}
	next := make(map[string]string, len(s.data)-1)
	for k, v := range s.data {
		if k != name {
			next[k] = v
		}
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// persist writes data to the ConfigMap, creating it if absent.
func (s *configMapTierStore) persist(data map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace},
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	cm.Data = data
	_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}
