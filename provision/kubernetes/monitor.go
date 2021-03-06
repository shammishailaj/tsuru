// Copyright 2019 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package kubernetes

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/router/rebuild"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	v1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/informers/internalinterfaces"
	"k8s.io/client-go/tools/cache"
)

const (
	informerSyncTimeout = 10 * time.Second
)

type clusterController struct {
	mu              sync.Mutex
	cluster         *ClusterClient
	informerFactory informers.SharedInformerFactory
	podInformer     v1informers.PodInformer
	serviceInformer v1informers.ServiceInformer
	nodeInformer    v1informers.NodeInformer
	stopCh          chan struct{}
}

func initAllControllers(p *kubernetesProvisioner) error {
	return forEachCluster(func(client *ClusterClient) error {
		_, err := getClusterController(p, client)
		return err
	})
}

func getClusterController(p *kubernetesProvisioner, cluster *ClusterClient) (*clusterController, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clusterControllers[cluster.Name]; ok {
		return c, nil
	}
	c := &clusterController{
		cluster: cluster,
		stopCh:  make(chan struct{}),
	}
	err := c.start()
	if err != nil {
		return nil, err
	}
	p.clusterControllers[cluster.Name] = c
	return c, nil
}

func stopClusterController(p *kubernetesProvisioner, cluster *ClusterClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clusterControllers[cluster.Name]; ok {
		c.stop()
	}
	delete(p.clusterControllers, cluster.Name)
}

func (c *clusterController) stop() {
	close(c.stopCh)
}

func (c *clusterController) start() error {
	informer, err := c.getPodInformerWait(false)
	if err != nil {
		return err
	}
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			err := c.onAdd(obj)
			if err != nil {
				log.Errorf("[router-update-controller] error on add pod event: %v", err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			err := c.onUpdate(oldObj, newObj)
			if err != nil {
				log.Errorf("[router-update-controller] error on update pod event: %v", err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			err := c.onDelete(obj)
			if err != nil {
				log.Errorf("[router-update-controller] error on delete pod event: %v", err)
			}
		},
	})
	return nil
}

func (c *clusterController) onAdd(obj interface{}) error {
	// Pods are never ready on add, ignore and do nothing
	return nil
}

func (c *clusterController) onUpdate(oldObj, newObj interface{}) error {
	newPod := oldObj.(*apiv1.Pod)
	oldPod := newObj.(*apiv1.Pod)
	if newPod.ResourceVersion == oldPod.ResourceVersion {
		return nil
	}
	c.addPod(newPod)
	return nil
}

func (c *clusterController) onDelete(obj interface{}) error {
	if pod, ok := obj.(*apiv1.Pod); ok {
		c.addPod(pod)
		return nil
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return errors.Errorf("couldn't get object from tombstone %#v", obj)
	}
	pod, ok := tombstone.Obj.(*apiv1.Pod)
	if !ok {
		return errors.Errorf("tombstone contained object that is not a Pod: %#v", obj)
	}
	c.addPod(pod)
	return nil
}

func (c *clusterController) addPod(pod *apiv1.Pod) {
	labelSet := labelSetFromMeta(&pod.ObjectMeta)
	appName := labelSet.AppName()
	if appName == "" {
		return
	}
	if labelSet.IsDeploy() || labelSet.IsIsolatedRun() {
		return
	}
	routerLocal, _ := c.cluster.RouterAddressLocal(labelSet.AppPool())
	if routerLocal {
		rebuild.EnqueueRoutesRebuild(appName)
	}
}

func (c *clusterController) getPodInformer() (v1informers.PodInformer, error) {
	return c.getPodInformerWait(true)
}

func (c *clusterController) getServiceInformer() (v1informers.ServiceInformer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.serviceInformer == nil {
		err := c.withInformerFactory(func(factory informers.SharedInformerFactory) {
			c.serviceInformer = factory.Core().V1().Services()
			c.serviceInformer.Informer()
		})
		if err != nil {
			return nil, err
		}
	}
	err := c.waitForSync(c.serviceInformer.Informer())
	return c.serviceInformer, err
}

func (c *clusterController) getNodeInformer() (v1informers.NodeInformer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nodeInformer == nil {
		err := c.withInformerFactory(func(factory informers.SharedInformerFactory) {
			c.nodeInformer = factory.Core().V1().Nodes()
			c.nodeInformer.Informer()
		})
		if err != nil {
			return nil, err
		}
	}
	err := c.waitForSync(c.nodeInformer.Informer())
	return c.nodeInformer, err
}

func (c *clusterController) getPodInformerWait(wait bool) (v1informers.PodInformer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.podInformer == nil {
		err := c.withInformerFactory(func(factory informers.SharedInformerFactory) {
			c.podInformer = factory.Core().V1().Pods()
			c.podInformer.Informer()
		})
		if err != nil {
			return nil, err
		}
	}
	var err error
	if wait {
		err = c.waitForSync(c.podInformer.Informer())
	}
	return c.podInformer, err
}

func (c *clusterController) withInformerFactory(fn func(factory informers.SharedInformerFactory)) error {
	factory, err := c.getFactory()
	if err != nil {
		return err
	}
	fn(factory)
	factory.Start(c.stopCh)
	return nil
}

func (c *clusterController) getFactory() (informers.SharedInformerFactory, error) {
	if c.informerFactory != nil {
		return c.informerFactory, nil
	}
	var err error
	c.informerFactory, err = InformerFactory(c.cluster)
	return c.informerFactory, err
}

func contextWithCancelByChannel(ctx context.Context, ch chan struct{}, timeout time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
			return
		}
	}()
	return ctx, cancel
}

func (c *clusterController) waitForSync(informer cache.SharedInformer) error {
	if informer.HasSynced() {
		return nil
	}
	ctx, cancel := contextWithCancelByChannel(context.Background(), c.stopCh, informerSyncTimeout)
	defer cancel()
	cache.WaitForCacheSync(ctx.Done(), informer.HasSynced)
	return errors.Wrap(ctx.Err(), "error waiting for informer sync")
}

var InformerFactory = func(client *ClusterClient) (informers.SharedInformerFactory, error) {
	timeout := client.restConfig.Timeout
	restConfig := *client.restConfig
	restConfig.Timeout = 0
	cli, err := ClientForConfig(&restConfig)
	if err != nil {
		return nil, err
	}
	tweakFunc := internalinterfaces.TweakListOptionsFunc(func(opts *metav1.ListOptions) {
		if opts.TimeoutSeconds == nil {
			timeoutSec := int64(timeout.Seconds())
			opts.TimeoutSeconds = &timeoutSec
		}
	})
	return informers.NewFilteredSharedInformerFactory(cli, time.Minute, metav1.NamespaceAll, tweakFunc), nil
}
