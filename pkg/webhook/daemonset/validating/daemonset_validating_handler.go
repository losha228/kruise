/*
Copyright 2020 The Kruise Authors.

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

package validating

import (
	"context"
	"net/http"

	genericvalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/apps"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ValidateDaemonSetName can be used to check whether the given daemon set name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateDaemonSetName = genericvalidation.NameIsDNSSubdomain

// DaemonSetCreateUpdateHandler handles DaemonSet
type DaemonSetCreateUpdateHandler struct {
	// Decoder decodes objects
	Decoder *admission.Decoder
}

func (h *DaemonSetCreateUpdateHandler) validatingDaemonSetFn(ctx context.Context, obj *apps.DaemonSet) (bool, string, error) {
	// allErrs := validateDaemonSet(obj)
	if obj.Namespace == "sonic" {
		fldPath := field.NewPath("spec")
		allErrs := validateDaemonSetUpdateStrategy(&obj.Spec.UpdateStrategy, fldPath.Child("updateStrategy"))
		if len(allErrs) != 0 {
			return false, "", allErrs.ToAggregate()
		}
	}
	return true, "allowed to be admitted", nil
}

func validateDaemonSetUpdateStrategy(strategy *apps.DaemonSetUpdateStrategy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if strategy.Type != apps.OnDeleteDaemonSetStrategyType {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("rollingUpdate"), strategy.Type, "Only OnDelete is supported!"))
		return allErrs
	}

	return allErrs
}

var _ admission.Handler = &DaemonSetCreateUpdateHandler{}

// Handle handles admission requests.
func (h *DaemonSetCreateUpdateHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &apps.DaemonSet{}

	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	allowed, reason, err := h.validatingDaemonSetFn(ctx, obj)
	if err != nil {
		klog.Warningf("ds %s/%s action %v fail:%s", obj.Namespace, obj.Name, req.AdmissionRequest.Operation, err.Error())
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.ValidationResponse(allowed, reason)
}

var _ admission.DecoderInjector = &DaemonSetCreateUpdateHandler{}

// InjectDecoder injects the decoder into the DaemonSetCreateUpdateHandler
func (h *DaemonSetCreateUpdateHandler) InjectDecoder(d *admission.Decoder) error {
	h.Decoder = d
	return nil
}
