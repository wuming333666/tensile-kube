/*
 * Copyright ©2020. The virtual-kubelet authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package webhook

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"

	"github.com/virtual-kubelet/tensile-kube/pkg/util"
)

var (
	freezeCache   = util.NewUnschedulableCache()
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
	// (https://github.com/kubernetes/kubernetes/issues/57982)
	defaulter  = runtime.ObjectDefaulter(runtimeScheme)
	desiredMap = map[string]corev1.Toleration{
		util.TaintNodeNotReady: {
			Key:      util.TaintNodeNotReady,
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoExecute,
		},
		util.TaintNodeUnreachable: {
			Key:      util.TaintNodeUnreachable,
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoExecute,
		},
	}
)

// HookServer is an interface defines a server
type HookServer interface {
	// Serve starts a server
	Serve(http.ResponseWriter, *http.Request)
}

// webhookServer is a sever for webhook
type webhookServer struct {
	ignoreSelectorKeys []string
	pvcLister          v1.PersistentVolumeClaimLister
	Server             *http.Server
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
}

// NewWebhookServer start a new webhook server
func NewWebhookServer(pvcLister v1.PersistentVolumeClaimLister, ignoreKeys []string) HookServer {
	return &webhookServer{
		ignoreSelectorKeys: ignoreKeys,
		pvcLister:          pvcLister,
	}
}

// mutate k8s pod annotations, Affinity, nodeSelector and etc.
func (whsvr *webhookServer) mutate(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var (
		err error
		pod corev1.Pod
	)
	switch req.Kind.Kind {
	case "Pod":
		rawBytes := req.Object.Raw
		klog.V(4).Infof("Raw request %v", string(rawBytes))
		if err := json.Unmarshal(rawBytes, &pod); err != nil {
			klog.Errorf("Could not unmarshal raw object %v err: %v", req, err)
			return &v1beta1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
	default:
		return &v1beta1.AdmissionResponse{
			Allowed: false,
		}
	}
	if shouldSkip(&pod) {
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}
	ref := getOwnerRef(&pod)
	clone := pod.DeepCopy()
	switch req.Operation {
	case v1beta1.Update:
		setUnschedulableNodes(ref, clone)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	case v1beta1.Create:
		nodes := getUnschedulableNodes(ref, clone)
		if len(nodes) > 0 {
			klog.Infof("Create pod %v Not nodes %+v", clone.Name, nodes)
			clone.Spec.Affinity, _ = util.ReplacePodNodeNameNodeAffinity(clone.Spec.Affinity, ref, 0, nil, nodes...)
		}
	default:
		klog.Warningf("Skip operation: %v", req.Operation)
	}

	whsvr.trySetNodeName(clone)
	inject(clone, whsvr.ignoreSelectorKeys)
	patch, err := util.CreateJSONPatch(pod, clone)
	klog.Infof("Final patch %+v", string(patch))
	var result metav1.Status
	if err != nil {
		result.Code = 403
		result.Message = err.Error()
	}
	jsonPatch := v1beta1.PatchTypeJSONPatch
	return &v1beta1.AdmissionResponse{
		Allowed:   true,
		Result:    &result,
		Patch:     patch,
		PatchType: &jsonPatch,
	}
}

// Serve method for webhook server
func (whsvr *webhookServer) Serve(w http.ResponseWriter, r *http.Request) {
	admissionReview, err := getRequestReview(r)
	if err != nil {
		klog.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	admissionResponse := whsvr.mutate(admissionReview)
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		admissionReview.Response.UID = admissionReview.Request.UID
	}
	resp, err := json.Marshal(admissionReview)
	if err != nil {
		klog.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		return
	}
	if _, err := w.Write(resp); err != nil {
		klog.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}

func (whsvr *webhookServer) trySetNodeName(pod *corev1.Pod) {
	if pod.Spec.Volumes == nil {
		return
	}
	nodeName := ""
	for _, volume := range pod.Spec.Volumes {
		pvcSource := volume.PersistentVolumeClaim
		if pvcSource == nil {
			continue
		}
		nodeName = whsvr.getNodeNameFromPVC(pod.Namespace, pvcSource.ClaimName)
		if len(nodeName) != 0 {
			pod.Spec.NodeName = nodeName
			klog.Infof("Set desired node name to %v ", nodeName)
			return
		}
	}
	return
}

func (whsvr *webhookServer) getNodeNameFromPVC(ns, pvcName string) string {
	var nodeName string
	pvc, err := whsvr.pvcLister.PersistentVolumeClaims(ns).Get(pvcName)
	if err != nil {
		return nodeName
	}
	if pvc.Annotations == nil {
		return nodeName
	}
	return pvc.Annotations[util.SelectedNodeKey]
}

func inject(pod *corev1.Pod, ignoreKeys []string) {
	nodeSelector := make(map[string]string)
	var affinity *corev1.Affinity

	if skipInject(pod) {
		return
	}

	if pod.Spec.Affinity != nil {
		affinity = injectAffinity(pod.Spec.Affinity, ignoreKeys)
	}

	if pod.Spec.NodeSelector != nil {
		nodeSelector = injectNodeSelector(pod.Spec.NodeSelector, ignoreKeys)
	}

	cns := util.ClustersNodeSelection{
		NodeSelector: nodeSelector,
		Affinity:     affinity,
		Tolerations:  pod.Spec.Tolerations,
	}
	cnsByte, err := json.Marshal(cns)
	if err != nil {
		return
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[util.SelectorKey] = string(cnsByte)

	pod.Spec.Tolerations = getPodTolerations(pod)
}

func getPodTolerations(pod *corev1.Pod) []corev1.Toleration {
	var notReady, unSchedulable bool
	tolerations := make([]corev1.Toleration, 0)
	for _, toleration := range pod.Spec.Tolerations {
		if toleration.Key == util.TaintNodeNotReady {
			notReady = true
		}
		if toleration.Key == util.TaintNodeUnreachable {
			unSchedulable = true
		}

		if _, found := desiredMap[toleration.Key]; found {
			tolerations = append(tolerations, desiredMap[toleration.Key])
			continue
		}
		tolerations = append(tolerations, toleration)
	}
	return addDefaultPodTolerations(tolerations, notReady, unSchedulable)
}

func addDefaultPodTolerations(tolerations []corev1.Toleration, notReady, unSchedulable bool) []corev1.Toleration {
	if !notReady {
		tolerations = append(tolerations, desiredMap[util.TaintNodeNotReady])
	}
	if !unSchedulable {
		tolerations = append(tolerations, desiredMap[util.TaintNodeUnreachable])
	}
	return tolerations
}

// injectNodeSelector reserve  ignoreLabels in nodeSelector, others would be removed
func injectNodeSelector(nodeSelector map[string]string, ignoreLabels []string) map[string]string {
	nodeSelectorBackup := make(map[string]string)
	finalNodeSelector := make(map[string]string)
	labelMap := make(map[string]string)
	for _, v := range ignoreLabels {
		labelMap[v] = v
	}
	for k, v := range nodeSelector {
		nodeSelectorBackup[k] = v
	}
	for k, v := range nodeSelector {
		// not found in label, delete
		if labelMap[k] != "" {
			continue
		}
		delete(nodeSelector, k)
		finalNodeSelector[k] = v
	}
	return finalNodeSelector
}

func injectAffinity(affinity *corev1.Affinity, ignoreLabels []string) *corev1.Affinity {
	labelMap := make(map[string]string)
	for _, v := range ignoreLabels {
		labelMap[v] = v
	}
	if affinity.NodeAffinity == nil {
		return nil
	}
	if affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	required := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		return nil
	}
	requiredCopy := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.DeepCopy()
	var nodeSelectorTerm []corev1.NodeSelectorTerm
	for termIdx, term := range requiredCopy.NodeSelectorTerms {
		var mes, mfs []corev1.NodeSelectorRequirement
		var mesDeleteCount, mfsDeleteCount int
		for meIdx, me := range term.MatchExpressions {
			if labelMap[me.Key] != "" {
				// found key, do not delete
				continue
			}
			mes = append(mes, *me.DeepCopy())

			required.
				NodeSelectorTerms[termIdx].MatchExpressions = append(required.
				NodeSelectorTerms[termIdx].MatchExpressions[:meIdx-mesDeleteCount], required.
				NodeSelectorTerms[termIdx].MatchExpressions[meIdx-mesDeleteCount+1:]...)
			mesDeleteCount++
		}

		for mfIdx, mf := range term.MatchFields {
			if labelMap[mf.Key] != "" {
				// found key, do not delete
				continue
			}

			mfs = append(mfs, *mf.DeepCopy())
			required.
				NodeSelectorTerms[termIdx].MatchFields = append(required.
				NodeSelectorTerms[termIdx].MatchFields[:mfIdx-mesDeleteCount],
				required.NodeSelectorTerms[termIdx].MatchFields[mfIdx-mfsDeleteCount+1:]...)
			mfsDeleteCount++
		}
		if len(mfs) != 0 || len(mes) != 0 {
			nodeSelectorTerm = append(nodeSelectorTerm, corev1.NodeSelectorTerm{MatchFields: mfs, MatchExpressions: mes})
		}
	}

	filteredTerms := make([]corev1.NodeSelectorTerm, 0)
	for _, term := range required.NodeSelectorTerms {
		if len(term.MatchFields) == 0 && len(term.MatchExpressions) == 0 {
			continue
		}
		filteredTerms = append(filteredTerms, term)
	}
	if len(filteredTerms) == 0 {
		required = nil
	} else {
		required.NodeSelectorTerms = filteredTerms
	}
	affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = required
	if len(nodeSelectorTerm) == 0 {
		return nil
	}
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: nodeSelectorTerm},
	}}
}

func shouldSkip(pod *corev1.Pod) bool {
	if pod.Namespace == "kube-system" {
		return true
	}
	if pod.Labels != nil {
		if pod.Labels[util.CreatedbyDescheduler] == "true" {
			return true
		}
		if !util.IsVirtualPod(pod) {
			return true
		}
	}
	return false
}

func skipInject(pod *corev1.Pod) bool {
	return len(pod.Spec.NodeSelector) == 0 &&
		pod.Spec.Affinity == nil &&
		pod.Spec.Tolerations == nil
}

func getOwnerRef(pod *corev1.Pod) string {
	ref := ""
	if len(pod.OwnerReferences) > 0 {
		ref = string(pod.OwnerReferences[0].UID)
	}
	return ref
}

func setUnschedulableNodes(ref string, pod *corev1.Pod) {
	node := ""
	if len(ref) == 0 {
		return
	}
	if pod.Annotations != nil {
		node = pod.Annotations["unschedulable-node"]
	}
	if len(node) > 0 {
		klog.Infof("Unschedulable nodes %+v ref %v to cache", node, ref)
		freezeCache.Add(node, ref)
	}
}

func getUnschedulableNodes(ref string, pod *corev1.Pod) []string {
	var nodes []string
	if len(ref) == 0 {
		return nodes
	}
	if len(pod.Spec.NodeName) != 0 {
		return nodes
	}
	nodes = freezeCache.GetFreezeNodes(ref)
	klog.Infof("Not in nodes %v for %v", nodes, ref)
	return nodes
}

func getRequestReview(r *http.Request) (*v1beta1.AdmissionReview, error) {
	if r.Body == nil {
		return nil, fmt.Errorf("empty body")
	}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	klog.V(5).Infof("Receive request: %+v", *r)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	ar := v1beta1.AdmissionReview{}
	if deserializer.Decode(body, nil, &ar); err != nil {
		return nil, fmt.Errorf("Can't decode body: %v", err)
	}
	return &ar, nil
}
