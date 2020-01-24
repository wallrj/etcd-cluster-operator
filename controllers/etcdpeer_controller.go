package controllers

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	etcdv1alpha1 "github.com/improbable-eng/etcd-cluster-operator/api/v1alpha1"
	"github.com/improbable-eng/etcd-cluster-operator/internal/etcdenvvar"
)

// EtcdPeerReconciler reconciles a EtcdPeer object
type EtcdPeerReconciler struct {
	client.Client
	Log logr.Logger
}

const (
	etcdImage           = "quay.io/coreos/etcd:v3.2.28"
	etcdScheme          = "http"
	peerLabel           = "etcd.improbable.io/peer-name"
	pvcCleanupFinalizer = "etcdpeer.etcd.improbable.io/pvc-cleanup"
)

// +kubebuilder:rbac:groups=etcd.improbable.io,resources=etcdpeers,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=etcd.improbable.io,resources=etcdpeers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=list;get;create;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=list;get;create;watch;delete

func initialMemberURL(member etcdv1alpha1.InitialClusterMember) *url.URL {
	return &url.URL{
		Scheme: etcdScheme,
		Host:   fmt.Sprintf("%s:%d", member.Host, etcdPeerPort),
	}
}

// staticBootstrapInitialCluster returns the value of `ETCD_INITIAL_CLUSTER`
// environment variable.
func staticBootstrapInitialCluster(static etcdv1alpha1.StaticBootstrap) string {
	s := make([]string, len(static.InitialCluster))
	// Put our peers in as the other entries
	for i, member := range static.InitialCluster {
		s[i] = fmt.Sprintf("%s=%s",
			member.Name,
			initialMemberURL(member).String())
	}
	return strings.Join(s, ",")
}

// advertiseURL builds the canonical URL of this peer from it's name and the
// cluster name.
func advertiseURL(etcdPeer etcdv1alpha1.EtcdPeer, port int32) *url.URL {
	return &url.URL{
		Scheme: etcdScheme,
		Host: fmt.Sprintf(
			"%s.%s.%s.svc:%d",
			etcdPeer.Name,
			etcdPeer.Spec.ClusterName,
			etcdPeer.Namespace,
			port,
		),
	}
}

func bindAllAddress(port int) *url.URL {
	return &url.URL{
		Scheme: etcdScheme,
		Host:   fmt.Sprintf("0.0.0.0:%d", port),
	}
}

func clusterStateValue(cs etcdv1alpha1.InitialClusterState) string {
	if cs == etcdv1alpha1.InitialClusterStateNew {
		return "new"
	} else if cs == etcdv1alpha1.InitialClusterStateExisting {
		return "existing"
	} else {
		return ""
	}
}

// goMaxProcs calculates an appropriate Golang thread limit (GOMAXPROCS) for the
// configured CPU limit.
//
// GOMAXPROCS defaults to the number of CPUs on the Kubelet host which may be
// much higher than the requests and limits defined for the pod,
// See https://github.com/golang/go/issues/33803
// If resources have been set and if CPU limit is > 0 then set GOMAXPROCS to an
// integer between 1 and floor(cpuLimit).
// Etcd might one day set its own GOMAXPROCS based on CPU quota:
// See: https://github.com/etcd-io/etcd/issues/11508
func goMaxProcs(cpuLimit resource.Quantity) *int64 {
	switch cpuLimit.Sign() {
	case -1, 0:
		return nil
	}
	goMaxProcs := cpuLimit.MilliValue() / 1000
	if goMaxProcs < 1 {
		goMaxProcs = 1
	}
	return pointer.Int64Ptr(goMaxProcs)
}

func defineReplicaSet(peer *etcdv1alpha1.EtcdPeer, log logr.Logger) *appsv1.ReplicaSet {
	var replicas int32 = 1

	// We use the same labels for the replica set itself, the selector on
	// the replica set, and the pod template under the replica set.
	labels := map[string]string{
		appLabel:     appName,
		clusterLabel: peer.Spec.ClusterName,
		peerLabel:    peer.Name,
	}

	etcdContainer := corev1.Container{
		Name:  appName,
		Image: etcdImage,
		Env: []corev1.EnvVar{
			{
				Name:  etcdenvvar.InitialCluster,
				Value: staticBootstrapInitialCluster(*peer.Spec.Bootstrap.Static),
			},
			{
				Name:  etcdenvvar.Name,
				Value: peer.Name,
			},
			{
				Name:  etcdenvvar.InitialClusterToken,
				Value: peer.Spec.ClusterName,
			},
			{
				Name:  etcdenvvar.InitialAdvertisePeerURLs,
				Value: advertiseURL(*peer, etcdPeerPort).String(),
			},
			{
				Name:  etcdenvvar.AdvertiseClientURLs,
				Value: advertiseURL(*peer, etcdClientPort).String(),
			},
			{
				Name:  etcdenvvar.ListenPeerURLs,
				Value: bindAllAddress(etcdPeerPort).String(),
			},
			{
				Name:  etcdenvvar.ListenClientURLs,
				Value: bindAllAddress(etcdClientPort).String(),
			},
			{
				Name:  etcdenvvar.InitialClusterState,
				Value: clusterStateValue(peer.Spec.Bootstrap.InitialClusterState),
			},
			{
				Name:  etcdenvvar.DataDir,
				Value: etcdDataMountPath,
			},
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "etcd-client",
				ContainerPort: etcdClientPort,
			},
			{
				Name:          "etcd-peer",
				ContainerPort: etcdPeerPort,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "etcd-data",
				MountPath: etcdDataMountPath,
			},
		},
	}
	if peer.Spec.PodTemplate != nil {
		if peer.Spec.PodTemplate.Resources != nil {
			etcdContainer.Resources = *peer.Spec.PodTemplate.Resources.DeepCopy()
			if value := goMaxProcs(*etcdContainer.Resources.Limits.Cpu()); value != nil {
				etcdContainer.Env = append(
					etcdContainer.Env,
					corev1.EnvVar{
						Name:  "GOMAXPROCS",
						Value: fmt.Sprintf("%d", *value),
					},
				)
			}
		}
	}
	replicaSet := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Labels:          labels,
			Annotations:     make(map[string]string),
			Name:            peer.Name,
			Namespace:       peer.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(peer, etcdv1alpha1.GroupVersion.WithKind("EtcdPeer"))},
		},
		Spec: appsv1.ReplicaSetSpec{
			// This will *always* be 1. Other peers are handled by other EtcdPeers.
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: make(map[string]string),
					Name:        peer.Name,
					Namespace:   peer.Namespace,
				},
				Spec: corev1.PodSpec{
					Hostname:   peer.Name,
					Subdomain:  peer.Spec.ClusterName,
					Containers: []corev1.Container{etcdContainer},
					Volumes: []corev1.Volume{
						{
							Name: "etcd-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: peer.Name,
								},
							},
						},
					},
				},
			},
		},
	}

	if peer.Spec.PodTemplate != nil {
		if peer.Spec.PodTemplate.Metadata != nil {
			// Stamp annotations
			for name, value := range peer.Spec.PodTemplate.Metadata.Annotations {
				if !etcdv1alpha1.IsInvalidUserProvidedAnnotationName(name) {
					if _, found := replicaSet.Spec.Template.Annotations[name]; !found {
						replicaSet.Spec.Template.Annotations[name] = value
					} else {
						// This will only check against an annotation that we set ourselves.
						log.V(2).Info("Ignoring annotation, we already have one with that name",
							"annotation-name", name)
					}
				} else {
					// In theory, this code is unreachable as we check this validation at the start of the reconcile
					// loop. See https://xkcd.com/2200
					log.V(2).Info("Ignoring annotation, applying etcd.improbable.io/ annotations is not supported",
						"annotation-name", name)
				}
			}
		}
	}

	return &replicaSet
}

func pvcForPeer(peer *etcdv1alpha1.EtcdPeer) *corev1.PersistentVolumeClaim {
	labels := map[string]string{
		appLabel:     appName,
		clusterLabel: peer.Spec.ClusterName,
		peerLabel:    peer.Name,
	}

	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      peer.Name,
			Namespace: peer.Namespace,
			Labels:    labels,
		},
		Spec: *peer.Spec.Storage.VolumeClaimTemplate.DeepCopy(),
	}
}

func hasPvcDeletionFinalizer(peer *etcdv1alpha1.EtcdPeer) bool {
	return sets.NewString(peer.ObjectMeta.Finalizers...).Has(pvcCleanupFinalizer)
}

type State struct {
	peer              *etcdv1alpha1.EtcdPeer
	pvc               *corev1.PersistentVolumeClaim
	desiredPVC        *corev1.PersistentVolumeClaim
	replicaSet        *appsv1.ReplicaSet
	desiredReplicaSet *appsv1.ReplicaSet
}

type StateCollector struct {
	log    logr.Logger
	client client.Client
}

func (o *StateCollector) GetState(ctx context.Context, req ctrl.Request) (*State, error) {
	state := &State{}

	var peer etcdv1alpha1.EtcdPeer
	err := o.client.Get(ctx, req.NamespacedName, &peer)
	if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	if err == nil {
		state.peer = &peer
	}

	var pvc corev1.PersistentVolumeClaim
	err = o.client.Get(ctx, req.NamespacedName, &pvc)
	if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	if err == nil {
		state.pvc = &pvc
	}

	var replicaSet appsv1.ReplicaSet
	err = o.client.Get(ctx, req.NamespacedName, &replicaSet)
	if client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	if err == nil {
		state.replicaSet = &replicaSet
	}

	if state.peer != nil {
		state.peer.Default()
		state.desiredPVC = pvcForPeer(state.peer)
		state.desiredReplicaSet = defineReplicaSet(state.peer, o.log)
	}

	return state, nil
}

func (r *EtcdPeerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	log := r.Log.WithValues("peer", req.NamespacedName)

	sc := &StateCollector{log: log, client: r.Client}
	state, err := sc.GetState(ctx, req)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error while getting current state: %s", err)
	}

	if state.peer == nil {
		log.Info("EtcdPeer not found")
		return ctrl.Result{}, nil
	}

	// Validate in case a validating webhook has not been deployed
	if err := state.peer.ValidateCreate(); err != nil {
		log.Error(err, "invalid EtcdPeer")
		return ctrl.Result{}, nil
	}

	var action Action
	switch {
	case !state.peer.ObjectMeta.DeletionTimestamp.IsZero() && hasPvcDeletionFinalizer(state.peer):
		// Peer deleted and requires PVC cleanup
		action = &PeerPVCDeleter{log: log, client: r.Client, peer: state.peer}

	case !state.peer.ObjectMeta.DeletionTimestamp.IsZero():
		// Peer deleted, no PVC cleanup
		action = &NoopAction{}

	case state.pvc == nil:
		// Create PVC
		action = &CreateRuntimeObject{log: log, client: r.Client, obj: state.desiredPVC}

	case state.replicaSet == nil:
		// Create Replicaset
		action = &CreateRuntimeObject{log: log, client: r.Client, obj: state.desiredReplicaSet}
	}

	if action != nil {
		return ctrl.Result{}, action.Execute(ctx)
	}

	return ctrl.Result{}, nil
}

type pvcMapper struct{}

var _ handler.Mapper = &pvcMapper{}

// Map looks up the peer name label from the PVC and generates a reconcile
// request for *that* name in the namespace of the pvc.
// This mapper ensures that we only wake up the Reconcile function for changes
// to PVCs related to EtcdPeer resources.
// PVCs are deliberately not owned by the peer, to ensure that they are not
// garbage collected along with the peer.
// So we can't use OwnerReference handler here.
func (m *pvcMapper) Map(o handler.MapObject) []reconcile.Request {
	requests := []reconcile.Request{}
	labels := o.Meta.GetLabels()
	if peerName, found := labels[peerLabel]; found {
		requests = append(
			requests,
			reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      peerName,
					Namespace: o.Meta.GetNamespace(),
				},
			},
		)
	}
	return requests
}

func (r *EtcdPeerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&etcdv1alpha1.EtcdPeer{}).
		// Watch for changes to ReplicaSet resources that an EtcdPeer owns.
		Owns(&appsv1.ReplicaSet{}).
		// We can use a simple EnqueueRequestForObject handler here as the PVC
		// has the same name as the EtcdPeer resource that needs to be enqueued
		Watches(&source.Kind{Type: &corev1.PersistentVolumeClaim{}}, &handler.EnqueueRequestsFromMapFunc{
			ToRequests: &pvcMapper{},
		}).
		Complete(r)
}
