/*
Copyright 2021 The Kruise Authors.

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

package podreadiness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openkruise/kruise/pkg/client/clientset/versioned/scheme"
	kruiseutil "github.com/openkruise/kruise/pkg/util"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	//"sigs.k8s.io/controller-runtime/pkg/client"
	"github.com/openkruise/kruise/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	concurrentReconciles = 3
)

func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (*ReconcilePodReadiness, error) {
	genericClient := client.GetGenericClientWithName("hook-controller")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: genericClient.KubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "sonic-hook-checker-controller"})
	cacher := mgr.GetCache()
	dsInformer, err := cacher.GetInformerForKind(context.TODO(), apps.SchemeGroupVersion.WithKind("DaemonSet"))
	if err != nil {
		return nil, err
	}
	dsLister := appslisters.NewDaemonSetLister(dsInformer.(cache.SharedIndexInformer).GetIndexer())
	return &ReconcilePodReadiness{
		/// Client:     utilclient.NewClientFromManager(mgr, "pod-hook-controller"),
		dsLister:      dsLister,
		kubeClient:    genericClient.KubeClient,
		eventRecorder: recorder,
	}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *ReconcilePodReadiness) error {
	// Create a new controller
	c, err := controller.New("pod-hook-controller", mgr, controller.Options{Reconciler: r, MaxConcurrentReconciles: concurrentReconciles})
	if err != nil {
		return err
	}

	err = c.Watch(&source.Kind{Type: &v1.Pod{}}, &handler.EnqueueRequestForObject{}, predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod := e.Object.(*v1.Pod)
			return r.checkCondition(pod.Annotations)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			pod := e.ObjectNew.(*v1.Pod)
			return r.checkCondition(pod.Annotations)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcilePodReadiness{}

// ReconcilePodReadiness reconciles a Pod object
type ReconcilePodReadiness struct {
	// client.Client
	kubeClient clientset.Interface
	// dsLister can list/get daemonsets from the shared informer's store
	dsLister      appslisters.DaemonSetLister
	eventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;update;patch

func (r *ReconcilePodReadiness) Reconcile(_ context.Context, request reconcile.Request) (res reconcile.Result, err error) {
	start := time.Now()
	klog.V(3).Infof("Starting to process Pod %v", request.NamespacedName)
	defer func() {
		if err != nil {
			klog.Warningf("Failed to process Pod %v, elapsedTime %v, error: %v", request.NamespacedName, time.Since(start), err)
		} else {
			klog.Infof("Finish to process Pod %v, elapsedTime %v", request.NamespacedName, time.Since(start))
		}
	}()

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pod := &v1.Pod{}
		// err = r.Get(context.TODO(), request.NamespacedName, pod)
		pod, err := r.kubeClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Object not found, return.  Created objects are automatically garbage collected.
				// For additional cleanup logic use finalizers.
				return nil
			}
			// Error reading the object - requeue the request.
			return err
		}
		if pod.DeletionTimestamp != nil {
			return nil
		}

		// check precheck hook
		if precheck, prechckFound := pod.Annotations["Precheck"]; prechckFound {
			if !strings.EqualFold(precheck, "completed") {
				r.doCheck(pod, "Precheck")
			}

		}

		if postcheck, postchckFound := pod.Annotations["Postcheck"]; postchckFound {
			if !strings.EqualFold(postcheck, "completed") {
				r.doCheck(pod, "Postcheck")
			}
		}

		return nil
	})
	return reconcile.Result{}, err
}

func (r *ReconcilePodReadiness) doCheck(pod *v1.Pod, checkType string) error {

	// sleep 10 second and make check completed
	klog.V(4).Infof("%s for pod %s/%s is started", checkType, pod.Namespace, pod.Name)
	time.Sleep(time.Duration(10) * time.Second)
	r.updatePodAnnotation(pod, checkType, "completed")
	ds, err := r.getPodDaemonSets(pod)
	if err == nil {
		r.eventRecorder.Eventf(ds, v1.EventTypeNormal, fmt.Sprintf("Pod%sSuccess", checkType), fmt.Sprintf("The %v for pod %v was completed.", checkType, pod.Name))
	}

	klog.V(4).Infof("%s for pod %s/%s is completed", checkType, pod.Namespace, pod.Name)
	return nil
}

func (r *ReconcilePodReadiness) checkCondition(pod *v1.Pod) bool {
	if _, hookCheckerEnabled := pod.Annotations["hookCheckerEnabled"]; !hookCheckerEnabled {
		klog.V(4).Infof("No hookCheckerEnabled for pod %s/%s, skip", pod.Namespace, pod.Name)
		return false
	}

	precheck, prechckFound := pod.Annotations["Precheck"]
	postcheck, postchckFound := pod.Annotations["Postcheck"]
	if !prechckFound && !postchckFound {
		return false
	}

	if !strings.EqualFold(precheck, "Pending") && !strings.EqualFold(postcheck, "Pending") {
		klog.V(4).Infof("No pending hook for pod %s/%s, skip", pod.Namespace, pod.Name)
		return false
	}
	return true
}

func (r *ReconcilePodReadiness) updatePodAnnotation(pod *v1.Pod, key, value string) (updated bool, err error) {
	if pod == nil {
		return false, nil
	}

	pod = pod.DeepCopy()

	body := fmt.Sprintf(
		`{"metadata":{"annotations":{"%s":"%s"}}}`,
		key,
		value)

	r.kubeClient.CoreV1().Pods(pod.Namespace).Patch(context.TODO(), pod.Name, types.StrategicMergePatchType, []byte(body), metav1.PatchOptions{})

	return true, err
}

func (r *ReconcilePodReadiness) getPodDaemonSets(pod *v1.Pod) (*apps.DaemonSet, error) {
	if len(pod.Labels) == 0 {
		return nil, fmt.Errorf("no daemon sets found for pod %v because it has no labels", pod.Name)
	}

	dsList, err := r.dsLister.DaemonSets(pod.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var selector labels.Selector
	var daemonSets []*apps.DaemonSet
	for _, ds := range dsList {
		selector, err = kruiseutil.ValidatedLabelSelectorAsSelector(ds.Spec.Selector)
		if err != nil {
			// this should not happen if the DaemonSet passed validation
			return nil, err
		}

		// If a daemonSet with a nil or empty selector creeps in, it should match nothing, not everything.
		if selector.Empty() || !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		daemonSets = append(daemonSets, ds)
	}

	if len(daemonSets) == 0 {
		return nil, fmt.Errorf("could not find daemon set for pod %s in namespace %s with labels: %v", pod.Name, pod.Namespace, pod.Labels)
	}

	return daemonSets[0], nil
}
