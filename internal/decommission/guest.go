package decommission

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayAPIGroups are the API groups whose custom resources are swept after
// the Gateway objects themselves are gone. Only the Gateway kind owns
// external infrastructure; the rest is inert config, but custom operators
// may hook deletion of these CRs, so sweep them best-effort.
var gatewayAPIGroups = map[string]bool{
	"gateway.networking.k8s.io":   true,
	"gateway.envoyproxy.io":       true,
	"gateway.networking.x-k8s.io": true,
}

var (
	gatewayListGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GatewayList"}
	argoAppList    = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ApplicationList"}
	argoApp        = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}
)

// preclean runs phase 1 against the guest cluster and returns an error when
// the verdict is NOT CLEAN.
func (r *Runner) preclean(ctx context.Context) error {
	r.printf("Phase 1: preclean guest cluster %s (external-system cleanup)", r.cluster.Name)

	r.stopArgoCD(ctx)
	r.startRORPurge(ctx)
	r.deleteIngresses(ctx)
	r.deleteGateways(ctx)
	r.sweepGatewayAPIConfig(ctx)
	r.deleteLBServices(ctx)
	r.scaleDownPVCWorkloads(ctx)
	r.deletePVCsAndWaitVolumes(ctx)
	r.collectRORResult(ctx)

	return r.verifyPreclean(ctx)
}

// --- ArgoCD --------------------------------------------------------------

func (r *Runner) stopArgoCD(ctx context.Context) {
	var ns corev1.Namespace
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Name: "argocd"}, &ns); err != nil {
		r.printf("No argocd namespace found, skipping ArgoCD steps")
		return
	}
	r.printf("Stop ArgoCD reconciler controllers (so nothing self-heals mid-teardown)")

	stopped := false
	var sts appsv1.StatefulSet
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "argocd", Name: "argocd-application-controller"}, &sts); err == nil {
		if err := r.scaleTo(ctx, &sts, 0); err != nil {
			r.warnf("failed to scale down argocd-application-controller: %v", err)
		} else {
			stopped = true
		}
	} else {
		var dep appsv1.Deployment
		if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "argocd", Name: "argocd-application-controller"}, &dep); err == nil {
			if err := r.scaleTo(ctx, &dep, 0); err != nil {
				r.warnf("failed to scale down argocd-application-controller: %v", err)
			} else {
				stopped = true
			}
		} else {
			r.warnf("argocd-application-controller not found as StatefulSet or Deployment, skipping")
		}
	}
	var appset appsv1.Deployment
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "argocd", Name: "argocd-applicationset-controller"}, &appset); err == nil {
		if err := r.scaleTo(ctx, &appset, 0); err != nil {
			r.warnf("failed to scale down argocd-applicationset-controller: %v", err)
		}
	}
	if stopped {
		r.waitUntil(ctx, "argocd-application-controller stopped", 30*time.Second, 3*time.Second, func(ctx context.Context) (bool, error) {
			var s appsv1.StatefulSet
			if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "argocd", Name: "argocd-application-controller"}, &s); err == nil {
				return s.Status.ReadyReplicas == 0, nil
			}
			var d appsv1.Deployment
			if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "argocd", Name: "argocd-application-controller"}, &d); err == nil {
				return d.Status.ReadyReplicas == 0, nil
			}
			return true, nil
		})
	}

	// Belt-and-suspenders: strip syncPolicy from every Application in case a
	// controller restarts mid-teardown.
	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(argoAppList)
	if err := r.guest.List(ctx, apps, ctrlclient.InNamespace("argocd")); err != nil {
		if !meta.IsNoMatchError(err) {
			r.failf("could not list ArgoCD applications: %v", err)
		}
		return
	}
	r.printf("Turn off autosync for %d ArgoCD applications", len(apps.Items))
	for i := range apps.Items {
		app := &apps.Items[i]
		patch := []byte(`{"spec":{"syncPolicy":null}}`)
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(argoApp)
		u.SetNamespace(app.GetNamespace())
		u.SetName(app.GetName())
		if err := r.guest.Patch(ctx, u, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
			r.failf("failed to disable autosync for application %s: %v", app.GetName(), err)
		}
	}
}

// scaleTo sets spec.replicas on a Deployment or StatefulSet.
func (r *Runner) scaleTo(ctx context.Context, obj ctrlclient.Object, replicas int32) error {
	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	return r.guest.Patch(ctx, obj, ctrlclient.RawPatch(types.MergePatchType, patch))
}

// --- ingress / gateway / services -----------------------------------------

func (r *Runner) deleteIngresses(ctx context.Context) {
	r.printf("Delete all ingresses")
	var ingresses netv1.IngressList
	if err := r.guest.List(ctx, &ingresses); err != nil {
		r.failf("could not list ingresses: %v", err)
		return
	}
	for i := range ingresses.Items {
		ing := &ingresses.Items[i]
		r.printf("  Deleting ingress %s in %s", ing.Name, ing.Namespace)
		if err := ignoreNotFound(r.guest.Delete(ctx, ing)); err != nil {
			r.failf("failed to delete ingress %s/%s: %v", ing.Namespace, ing.Name, err)
		}
	}
}

// deleteGateways lets finalizers run — they are how gateway controllers
// clean up external resources (LB service, IPAM, DNS). Finalizers are
// stripped only as a last resort on stuck gateways, with a loud warning.
func (r *Runner) deleteGateways(ctx context.Context) {
	gws := &unstructured.UnstructuredList{}
	gws.SetGroupVersionKind(gatewayListGVK)
	if err := r.guest.List(ctx, gws); err != nil {
		if meta.IsNoMatchError(err) {
			r.printf("Gateway API not installed on this cluster, skipping gateway cleanup")
			return
		}
		r.failf("could not list gateways: %v", err)
		return
	}
	r.printf("Delete all Gateway objects (so controllers clean up owned LoadBalancer services)")
	for i := range gws.Items {
		gw := &gws.Items[i]
		if err := ignoreNotFound(r.guest.Delete(ctx, gw)); err != nil {
			r.failf("failed to delete gateway %s/%s: %v", gw.GetNamespace(), gw.GetName(), err)
		}
	}
	cleared := r.waitUntil(ctx, "gateways cleared", 120*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(gatewayListGVK)
		if err := r.guest.List(ctx, l); err != nil {
			return false, err
		}
		return len(l.Items) == 0, nil
	})
	if !cleared {
		r.warnf("some gateways stuck terminating — stripping finalizers as last resort.")
		r.warnf("external resources owned by these gateways may NOT have been cleaned up — verify manually!")
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(gatewayListGVK)
		if err := r.guest.List(ctx, l); err == nil {
			for i := range l.Items {
				gw := &l.Items[i]
				patch := []byte(`{"metadata":{"finalizers":[]}}`)
				if err := r.guest.Patch(ctx, gw, ctrlclient.RawPatch(types.MergePatchType, patch)); err != nil {
					r.warnf("failed to clear finalizers on gateway %s/%s: %v", gw.GetNamespace(), gw.GetName(), err)
				}
			}
		}
		if !r.waitUntil(ctx, "gateways cleared (after finalizer strip)", 30*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
			l := &unstructured.UnstructuredList{}
			l.SetGroupVersionKind(gatewayListGVK)
			if err := r.guest.List(ctx, l); err != nil {
				return false, err
			}
			return len(l.Items) == 0, nil
		}) {
			r.failed = true
		}
	}
}

// sweepGatewayAPIConfig deletes all instances of every CRD in the Gateway
// API groups except Gateway itself (handled above, gated). These CRs are
// inert config, but custom operators may do delete-time cleanup from them.
// Runs AFTER gateway deletion: deleting e.g. an EnvoyProxy while its Gateway
// exists makes the controller re-reconcile the LB service with defaults.
func (r *Runner) sweepGatewayAPIConfig(ctx context.Context) {
	var crds apiextv1.CustomResourceDefinitionList
	if err := r.guest.List(ctx, &crds); err != nil {
		r.warnf("could not list CRDs for gateway config sweep: %v", err)
		return
	}
	r.printf("Sweep remaining Gateway API config resources (routes, policies, proxies, classes)")
	for i := range crds.Items {
		crd := &crds.Items[i]
		if !gatewayAPIGroups[crd.Spec.Group] || crd.Spec.Names.Kind == "Gateway" {
			continue
		}
		var served string
		for _, v := range crd.Spec.Versions {
			if v.Served {
				served = v.Name
				break
			}
		}
		if served == "" {
			continue
		}
		gvk := schema.GroupVersionKind{Group: crd.Spec.Group, Version: served, Kind: crd.Spec.Names.ListKind}
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(gvk)
		if err := r.guest.List(ctx, l); err != nil || len(l.Items) == 0 {
			continue
		}
		r.printf("  Deleting all %s.%s (%d)", strings.ToLower(crd.Spec.Names.Plural), crd.Spec.Group, len(l.Items))
		for j := range l.Items {
			item := &l.Items[j]
			if err := ignoreNotFound(r.guest.Delete(ctx, item)); err != nil {
				r.warnf("failed to delete %s %s/%s: %v", crd.Spec.Names.Kind, item.GetNamespace(), item.GetName(), err)
			}
		}
	}
	r.printf("Done sweeping Gateway API config")
}

func (r *Runner) deleteLBServices(ctx context.Context) {
	r.printf("Delete any remaining LoadBalancer services")
	var svcs corev1.ServiceList
	if err := r.guest.List(ctx, &svcs); err != nil {
		r.failf("could not list services: %v", err)
		return
	}
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		r.printf("  Deleting service %s in %s", svc.Name, svc.Namespace)
		if err := ignoreNotFound(r.guest.Delete(ctx, svc)); err != nil {
			r.failf("failed to delete svc %s/%s: %v", svc.Namespace, svc.Name, err)
		}
	}
	r.waitUntil(ctx, "LoadBalancer services cleared", 60*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
		var l corev1.ServiceList
		if err := r.guest.List(ctx, &l); err != nil {
			return false, err
		}
		for i := range l.Items {
			if l.Items[i].Spec.Type == corev1.ServiceTypeLoadBalancer {
				return false, nil
			}
		}
		return true, nil
	})
}

// --- PVC workloads and volumes ---------------------------------------------

// scaleDownPVCWorkloads scales every controller whose pods mount a PVC to
// zero. When the workload is itself owned by an operator CR (Alertmanager,
// Prometheus, ...), the OWNER's spec.replicas is patched instead — scaling
// the workload directly is reverted by the operator's reconcile loop.
func (r *Runner) scaleDownPVCWorkloads(ctx context.Context) {
	r.printf("Scale down controllers (Deployments/StatefulSets) that use PVCs")
	var pods corev1.PodList
	if err := r.guest.List(ctx, &pods); err != nil {
		r.failf("could not list pods: %v", err)
		return
	}
	seen := map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podMountsPVC(pod) || len(pod.OwnerReferences) == 0 {
			if podMountsPVC(pod) {
				r.warnf("pod %s/%s uses a PVC but has no controller — cannot scale down", pod.Namespace, pod.Name)
			}
			continue
		}
		owner := pod.OwnerReferences[0]
		switch owner.Kind {
		case "ReplicaSet":
			var rs appsv1.ReplicaSet
			if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, &rs); err != nil {
				r.warnf("could not resolve Deployment for RS %s/%s: %v", pod.Namespace, owner.Name, err)
				continue
			}
			if len(rs.OwnerReferences) == 0 {
				continue
			}
			key := pod.Namespace + "/Deployment/" + rs.OwnerReferences[0].Name
			if seen[key] {
				continue
			}
			seen[key] = true
			r.printf("  Scaling down Deployment %s in %s (pod %s uses PVC)", rs.OwnerReferences[0].Name, pod.Namespace, pod.Name)
			var dep appsv1.Deployment
			if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: pod.Namespace, Name: rs.OwnerReferences[0].Name}, &dep); err != nil {
				r.warnf("could not get Deployment %s/%s: %v", pod.Namespace, rs.OwnerReferences[0].Name, err)
				continue
			}
			r.scaleWorkloadToZero(ctx, &dep, dep.OwnerReferences)
		case "StatefulSet":
			key := pod.Namespace + "/StatefulSet/" + owner.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			r.printf("  Scaling down StatefulSet %s in %s (pod %s uses PVC)", owner.Name, pod.Namespace, pod.Name)
			var sts appsv1.StatefulSet
			if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, &sts); err != nil {
				r.warnf("could not get StatefulSet %s/%s: %v", pod.Namespace, owner.Name, err)
				continue
			}
			r.scaleWorkloadToZero(ctx, &sts, sts.OwnerReferences)
		default:
			r.warnf("pod %s/%s uses a PVC but has unscalable controller %s/%s", pod.Namespace, pod.Name, owner.Kind, owner.Name)
		}
	}
	r.waitUntil(ctx, "PVC-mounting pods terminated", 90*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
		var l corev1.PodList
		if err := r.guest.List(ctx, &l); err != nil {
			return false, err
		}
		for i := range l.Items {
			if podMountsPVC(&l.Items[i]) && l.Items[i].Status.Phase == corev1.PodRunning {
				return false, nil
			}
		}
		return true, nil
	})
}

// scaleWorkloadToZero patches the workload's operator-CR owner when one
// exists (its reconcile loop would revert a direct scale), else scales the
// workload itself.
func (r *Runner) scaleWorkloadToZero(ctx context.Context, workload ctrlclient.Object, owners []metav1.OwnerReference) {
	if len(owners) > 0 {
		o := owners[0]
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.FromAPIVersionAndKind(o.APIVersion, o.Kind))
		u.SetNamespace(workload.GetNamespace())
		u.SetName(o.Name)
		patch := []byte(`{"spec":{"replicas":0}}`)
		r.printf("    %s is owned by %s/%s — patching owner replicas to 0 instead", workload.GetName(), o.Kind, o.Name)
		if err := r.guest.Patch(ctx, u, ctrlclient.RawPatch(types.MergePatchType, patch)); err == nil {
			return
		}
		r.warnf("%s/%s has no patchable spec.replicas — falling back to direct scale", o.Kind, o.Name)
	}
	if err := r.scaleTo(ctx, workload, 0); err != nil {
		r.warnf("scale failed for %s/%s: %v", workload.GetNamespace(), workload.GetName(), err)
	}
}

func podMountsPVC(pod *corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

// deletePVCsAndWaitVolumes deletes all PVCs, then waits for the CSI driver
// to detach (VolumeAttachments gone) and delete the external volumes (PVs
// gone). PVC deletion returning does NOT mean the backing volumes are gone;
// declaring success before the PVs clear leaks external storage.
func (r *Runner) deletePVCsAndWaitVolumes(ctx context.Context) {
	r.printf("Delete all PVCs")
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.guest.List(ctx, &pvcs); err != nil {
		r.failf("could not list PVCs: %v", err)
		return
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if err := ignoreNotFound(r.guest.Delete(ctx, pvc)); err != nil {
			r.failf("failed to delete pvc %s/%s: %v", pvc.Namespace, pvc.Name, err)
		}
	}
	r.waitUntil(ctx, "PVCs cleared", 60*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
		var l corev1.PersistentVolumeClaimList
		if err := r.guest.List(ctx, &l); err != nil {
			return false, err
		}
		return len(l.Items) == 0, nil
	})
	r.printf("Wait for volume detach and external volume deletion")
	r.waitUntil(ctx, "VolumeAttachments cleared (CSI detach)", 120*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
		var l storagev1.VolumeAttachmentList
		if err := r.guest.List(ctx, &l); err != nil {
			return false, err
		}
		return len(l.Items) == 0, nil
	})
	r.waitUntil(ctx, "PVs cleared (external volumes deleted)", 120*time.Second, 5*time.Second, func(ctx context.Context) (bool, error) {
		var l corev1.PersistentVolumeList
		if err := r.guest.List(ctx, &l); err != nil {
			return false, err
		}
		return len(l.Items) == 0, nil
	})
}

// --- final guest verification ------------------------------------------------

func (r *Runner) verifyPreclean(ctx context.Context) error {
	var problems []string

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.guest.List(ctx, &pvcs); err != nil {
		problems = append(problems, fmt.Sprintf("cannot verify PVCs: %v", err))
	} else {
		for i := range pvcs.Items {
			problems = append(problems, "remaining PVC: "+pvcs.Items[i].Namespace+"/"+pvcs.Items[i].Name)
		}
	}
	var pvs corev1.PersistentVolumeList
	if err := r.guest.List(ctx, &pvs); err != nil {
		problems = append(problems, fmt.Sprintf("cannot verify PVs: %v", err))
	} else {
		for i := range pvs.Items {
			problems = append(problems, "remaining PV (external volume possibly not deleted): "+pvs.Items[i].Name)
		}
	}
	var vas storagev1.VolumeAttachmentList
	if err := r.guest.List(ctx, &vas); err != nil {
		problems = append(problems, fmt.Sprintf("cannot verify VolumeAttachments: %v", err))
	} else {
		for i := range vas.Items {
			problems = append(problems, "remaining VolumeAttachment: "+vas.Items[i].Name)
		}
	}
	gws := &unstructured.UnstructuredList{}
	gws.SetGroupVersionKind(gatewayListGVK)
	if err := r.guest.List(ctx, gws); err == nil {
		for i := range gws.Items {
			problems = append(problems, "remaining Gateway: "+gws.Items[i].GetNamespace()+"/"+gws.Items[i].GetName())
		}
	} else if !meta.IsNoMatchError(err) {
		problems = append(problems, fmt.Sprintf("cannot verify Gateways: %v", err))
	}
	var svcs corev1.ServiceList
	if err := r.guest.List(ctx, &svcs); err != nil {
		problems = append(problems, fmt.Sprintf("cannot verify services: %v", err))
	} else {
		for i := range svcs.Items {
			if svcs.Items[i].Spec.Type == corev1.ServiceTypeLoadBalancer {
				problems = append(problems, "remaining LoadBalancer service: "+svcs.Items[i].Namespace+"/"+svcs.Items[i].Name)
			}
		}
	}

	if r.failed || len(problems) > 0 {
		for _, p := range problems {
			r.printf("  ❌ %s", p)
		}
		return fmt.Errorf("preclean NOT CLEAN — do not delete the cluster yet; review the warnings above")
	}
	r.printf("✅ Preclean verified clean")
	return nil
}

var _ = apierrors.IsNotFound // keep import used when build tags vary
