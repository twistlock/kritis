/*
Copyright 2018 Google LLC

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

package admission

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/grafeas/kritis/pkg/kritis/metadata/containeranalysis"
	"github.com/grafeas/kritis/pkg/kritis/metadata/grafeas"

	"github.com/golang/glog"
	"github.com/grafeas/kritis/cmd/kritis/version"
	"github.com/grafeas/kritis/pkg/kritis/admission/constants"
	kritisv1beta1 "github.com/grafeas/kritis/pkg/kritis/apis/kritis/v1beta1"
	kritisconstants "github.com/grafeas/kritis/pkg/kritis/constants"
	"github.com/grafeas/kritis/pkg/kritis/crd/authority"
	"github.com/grafeas/kritis/pkg/kritis/crd/securitypolicy"
	"github.com/grafeas/kritis/pkg/kritis/metadata"
	"github.com/grafeas/kritis/pkg/kritis/review"
	"github.com/grafeas/kritis/pkg/kritis/secrets"
	"github.com/grafeas/kritis/pkg/kritis/violation"
	"k8s.io/api/admission/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

type config struct {
	retrievePod                func(r *http.Request) (*v1.Pod, v1beta1.AdmissionReview, error)
	retrieveDeployment         func(r *http.Request) (*appsv1.Deployment, v1beta1.AdmissionReview, error)
	fetchMetadataClient        func(config *Config) (metadata.Fetcher, error)
	fetchImageSecurityPolicies func(namespace string) ([]kritisv1beta1.ImageSecurityPolicy, error)
	reviewer                   func(metadata.Fetcher) reviewer
}

var (
	// For testing
	admissionConfig = config{
		retrievePod:                unmarshalPod,
		retrieveDeployment:         unmarshalDeployment,
		fetchMetadataClient:        MetadataClient,
		fetchImageSecurityPolicies: securitypolicy.ImageSecurityPolicies,
		reviewer:                   getReviewer,
	}

	defaultViolationStrategy = &violation.LoggingStrategy{}
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
)

// Config is the metadata client configuration
type Config struct {
	Metadata string // Metadata is the name of the metadata client fetcher
}

// MetadataClient returns metadata.Fetcher based on the admission control config
func MetadataClient(config *Config) (metadata.Fetcher, error) {
	if config.Metadata == constants.GrafeasMetadata {
		return grafeas.New()
	}
	if config.Metadata == constants.ContainerAnalysisMetadata {
		return containeranalysis.NewCache()
	}
	return nil, fmt.Errorf("unsupported backend %v", config.Metadata)
}

var handlers = map[string]func(*v1beta1.AdmissionReview, *v1beta1.AdmissionReview, *Config) error{
	"Deployment": handleDeployment,
	"Pod":        handlePod,
	"ReplicaSet": handleReplicaSet,
}

func handleDeployment(ar *v1beta1.AdmissionReview, admitResponse *v1beta1.AdmissionReview, config *Config) error {
	deployment := appsv1.Deployment{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &deployment); err != nil {
		return err
	}
	glog.Infof("handling deployment %s...", deployment.Name)
	reviewDeployment(&deployment, admitResponse, config)
	return nil
}

func handlePod(ar *v1beta1.AdmissionReview, admitResponse *v1beta1.AdmissionReview, config *Config) error {
	pod := v1.Pod{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
		return err
	}
	glog.Infof("handling pod %s in...", pod.Name)
	reviewPod(&pod, admitResponse, config)
	return nil
}

func handleReplicaSet(ar *v1beta1.AdmissionReview, admitResponse *v1beta1.AdmissionReview, config *Config) error {
	replicaSet := appsv1.ReplicaSet{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &replicaSet); err != nil {
		return err
	}
	glog.Infof("handling replica set %s...", replicaSet.Name)
	reviewReplicaSet(&replicaSet, admitResponse, config)
	return nil
}

func deserializeRequest(w http.ResponseWriter, r *http.Request) (v1beta1.AdmissionReview, error) {
	ar := v1beta1.AdmissionReview{}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return ar, err
	}

	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)

		payload, err := json.Marshal(&v1beta1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Status:  string(constants.FailureStatus),
				Message: err.Error(),
			},
		})
		if err != nil {
			glog.Errorf("unable to marshal %s: %v", payload, err)
		}
		if _, err := w.Write(payload); err != nil {
			glog.Errorf("unable to write payload: %v", err)
		}
	}
	return ar, nil
}

func ReviewHandler(w http.ResponseWriter, r *http.Request, config *Config) {
	glog.Infof("Starting admission review handler\nversion: %s\ncommit: %s",
		version.Version,
		version.Commit,
	)
	ar, err := deserializeRequest(w, r)
	if err != nil {
		glog.Errorf("Error reading body: %v", err)
		http.Error(w, "can't read body", http.StatusBadRequest)
		return
	}

	if ar.Request == nil {
		http.Error(w, "admission request is missing", http.StatusBadRequest)
		return
	}

	admitResponse := &v1beta1.AdmissionReview{
		Response: &v1beta1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: true,
			Result: &metav1.Status{
				Status:  string(constants.SuccessStatus),
				Message: constants.SuccessMessage,
			},
		},
	}

	for k8sType, handler := range handlers {
		if ar.Request.Kind.Kind == k8sType {
			if err := handler(&ar, admitResponse, config); err != nil {
				glog.Errorf("handler failed: %v", err)
				http.Error(w, "Whoops! The handler failed!", http.StatusInternalServerError)
				return
			}

		}
	}

	// Send response
	w.Header().Set("Content-Type", "application/json")
	payload, err := json.Marshal(admitResponse)
	if err != nil {
		glog.Errorf("failed to marshal response: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		glog.Errorf("failed to write payload: %v", err)
	}
}

func reviewDeployment(deployment *appsv1.Deployment, ar *v1beta1.AdmissionReview, config *Config) {
	images := DeploymentImages(*deployment)
	// check if the Deployments's owner has already been validated
	if checkOwners(images, &deployment.ObjectMeta) {
		glog.Infof("all owners for Deployment %s have been validated, returning successful status", deployment.Name)
		return
	}
	// check for a breakglass annotation on the deployment
	if checkBreakglass(&deployment.ObjectMeta) {
		glog.Infof("found breakglass annotation for %s, returning successful status", deployment.Name)
		return
	}
	reviewImages(images, deployment.Namespace, nil, ar, config)
}

func createDeniedResponse(ar *v1beta1.AdmissionReview, message string) {
	ar.Response.Allowed = false
	ar.Response.Result = &metav1.Status{
		Status:  string(constants.FailureStatus),
		Message: message,
	}
}

func reviewImages(images []string, ns string, pod *v1.Pod, ar *v1beta1.AdmissionReview, config *Config) {
	// NOTE: pod may be nil if we are reviewing images for a replica set.
	glog.Infof("Reviewing images for %s in namespace %s: %s", pod, ns, images)
	isps, err := admissionConfig.fetchImageSecurityPolicies(ns)
	if err != nil {
		errMsg := fmt.Sprintf("error getting image security policies: %v", err)
		glog.Errorf(errMsg)
		createDeniedResponse(ar, errMsg)
		return
	}
	if len(isps) == 0 {
		glog.Errorf("No ISP's found in namespace %s", ns)
	} else {
		glog.Infof("Found %d ISPs to review image against", len(isps))
	}

	client, err := admissionConfig.fetchMetadataClient(config)
	if err != nil {
		errMsg := fmt.Sprintf("error getting metadata client: %v", err)
		glog.Errorf(errMsg)
		createDeniedResponse(ar, errMsg)
		return
	}
	r := admissionConfig.reviewer(client)
	if err := r.Review(images, isps, pod); err != nil {
		glog.Infof("Denying %s in namespace %s: %v", pod, ns, err)
		createDeniedResponse(ar, err.Error())
	}
}

func reviewPod(pod *v1.Pod, ar *v1beta1.AdmissionReview, config *Config) {
	images := PodImages(*pod)
	// check if the Pod's owner has already been validated
	if checkOwners(images, &pod.ObjectMeta) {
		glog.Infof("all owners for Pod %s have been validated, returning sucessful status", pod.Name)
		return
	}
	// check for a breakglass annotation on the pod
	if checkBreakglass(&pod.ObjectMeta) {
		glog.Infof("found breakglass annotation for %s, returning successful status", pod.Name)
		return
	}
	reviewImages(images, pod.Namespace, pod, ar, config)
}

func reviewReplicaSet(replicaSet *appsv1.ReplicaSet, ar *v1beta1.AdmissionReview, config *Config) {
	images := ReplicaSetImages(*replicaSet)
	// check if the ReplicaSet's owner has already been validated
	if checkOwners(images, &replicaSet.ObjectMeta) {
		glog.Infof("all owners for ReplicaSet %s have been validated, returning successful status", replicaSet.Name)
		return
	}
	// check for a breakglass annotation on the replica set
	if checkBreakglass(&replicaSet.ObjectMeta) {
		glog.Infof("found breakglass annotation for %s, returning successful status", replicaSet.Name)
		return
	}
	reviewImages(images, replicaSet.Namespace, nil, ar, config)
}

// TODO(aaron-prindle) remove these functions
func unmarshalPod(r *http.Request) (*v1.Pod, v1beta1.AdmissionReview, error) {
	ar := v1beta1.AdmissionReview{}
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, ar, err
	}
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, ar, err
	}
	pod := v1.Pod{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
		return nil, ar, err
	}
	return &pod, ar, nil
}

func unmarshalDeployment(r *http.Request) (*appsv1.Deployment, v1beta1.AdmissionReview, error) {
	ar := v1beta1.AdmissionReview{}
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, ar, err
	}
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, ar, err
	}
	deployment := appsv1.Deployment{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &deployment); err != nil {
		return nil, ar, err
	}
	return &deployment, ar, nil
}

func checkBreakglass(meta *metav1.ObjectMeta) bool {
	annotations := meta.GetAnnotations()
	if annotations == nil {
		return false
	}
	_, ok := annotations[kritisconstants.Breakglass]
	return ok
}

func getReviewer(client metadata.Fetcher) reviewer {
	return review.New(client, &review.Config{
		Strategy:  defaultViolationStrategy,
		IsWebhook: true,
		Secret:    secrets.Fetch,
		Auths:     authority.Authorities,
		Validate:  securitypolicy.ValidateImageSecurityPolicy,
	})
}

// reviewer interface defines an Kritis Reviewer Struct.
// TODO: This will be removed in future refactoring.
type reviewer interface {
	Review(images []string, isps []kritisv1beta1.ImageSecurityPolicy, pod *v1.Pod) error
}
