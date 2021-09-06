/*
Copyright 2021 The Kubernetes Authors.

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

package topology

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controllers/topology/internal/contract"
	"sigs.k8s.io/cluster-api/controllers/topology/internal/scope"
)

// computeDesiredState computes the desired state of the cluster topology.
// NOTE: We are assuming all the required objects are provided as input; also, in case of any error,
// the entire compute operation operation will fail. This might be improved in the future if support for reconciling
// subset of a topology will be implemented.
func (r *ClusterReconciler) computeDesiredState(ctx context.Context, s *scope.Scope) (*scope.ClusterState, error) {
	var err error
	desiredState := &scope.ClusterState{
		ControlPlane: &scope.ControlPlaneState{},
	}

	// Compute the desired state of the InfrastructureCluster object.
	if desiredState.InfrastructureCluster, err = computeInfrastructureCluster(ctx, s); err != nil {
		return nil, err
	}

	// If the clusterClass mandates the controlPlane has infrastructureMachines, compute the InfrastructureMachineTemplate for the ControlPlane.
	if s.Blueprint.HasControlPlaneInfrastructureMachine() {
		if desiredState.ControlPlane.InfrastructureMachineTemplate, err = computeControlPlaneInfrastructureMachineTemplate(ctx, s); err != nil {
			return nil, err
		}
	}

	// Compute the desired state of the ControlPlane object, eventually adding a reference to the
	// InfrastructureMachineTemplate generated by the previous step.
	if desiredState.ControlPlane.Object, err = computeControlPlane(ctx, s, desiredState.ControlPlane.InfrastructureMachineTemplate); err != nil {
		return nil, err
	}

	// Compute the desired state for the Cluster object adding a reference to the
	// InfrastructureCluster and the ControlPlane objects generated by the previous step.
	desiredState.Cluster = computeCluster(ctx, s, desiredState.InfrastructureCluster, desiredState.ControlPlane.Object)

	// If required by the blueprint, compute the desired state of the MachineDeployment objects for the worker nodes, if any.
	if !s.Blueprint.HasMachineDeployments() {
		return desiredState, nil
	}

	desiredState.MachineDeployments = map[string]*scope.MachineDeploymentState{}
	for _, mdTopology := range s.Blueprint.Topology.Workers.MachineDeployments {
		desiredMachineDeployment, err := computeMachineDeployment(ctx, s, mdTopology)
		if err != nil {
			return nil, err
		}
		desiredState.MachineDeployments[mdTopology.Name] = desiredMachineDeployment
	}
	return desiredState, nil
}

// computeInfrastructureCluster computes the desired state for the InfrastructureCluster object starting from the
// corresponding template defined in the blueprint.
func computeInfrastructureCluster(_ context.Context, s *scope.Scope) (*unstructured.Unstructured, error) {
	template := s.Blueprint.InfrastructureClusterTemplate
	templateClonedFromref := s.Blueprint.ClusterClass.Spec.Infrastructure.Ref
	cluster := s.Current.Cluster
	currentRef := cluster.Spec.InfrastructureRef

	infrastructureCluster, err := templateToObject(templateToInput{
		template:              template,
		templateClonedFromRef: templateClonedFromref,
		cluster:               cluster,
		namePrefix:            fmt.Sprintf("%s-", cluster.Name),
		currentObjectRef:      currentRef,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate the InfrastructureCluster object from the %s", template.GetKind())
	}
	return infrastructureCluster, nil
}

// computeControlPlaneInfrastructureMachineTemplate computes the desired state for InfrastructureMachineTemplate
// that should be referenced by the ControlPlane object.
func computeControlPlaneInfrastructureMachineTemplate(_ context.Context, s *scope.Scope) (*unstructured.Unstructured, error) {
	template := s.Blueprint.ControlPlane.InfrastructureMachineTemplate
	templateClonedFromRef := s.Blueprint.ClusterClass.Spec.ControlPlane.MachineInfrastructure.Ref
	cluster := s.Current.Cluster

	// Check if the current control plane object has a machineTemplate.infrastructureRef already defined.
	// TODO: Move the next few lines into a method on scope.ControlPlaneState
	var currentRef *corev1.ObjectReference
	if s.Current.ControlPlane != nil && s.Current.ControlPlane.Object != nil {
		var err error
		if currentRef, err = contract.ControlPlane().MachineTemplate().InfrastructureRef().Get(s.Current.ControlPlane.Object); err != nil {
			return nil, errors.Wrap(err, "failed to get spec.machineTemplate.infrastructureRef for the current ControlPlane object")
		}
	}

	controlPlaneInfrastructureMachineTemplate := templateToTemplate(templateToInput{
		template:              template,
		templateClonedFromRef: templateClonedFromRef,
		cluster:               cluster,
		namePrefix:            controlPlaneInfrastructureMachineTemplateNamePrefix(cluster.Name),
		currentObjectRef:      currentRef,
	})
	return controlPlaneInfrastructureMachineTemplate, nil
}

// computeControlPlane computes the desired state for the ControlPlane object starting from the
// corresponding template defined in the blueprint.
func computeControlPlane(_ context.Context, s *scope.Scope, infrastructureMachineTemplate *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	template := s.Blueprint.ControlPlane.Template
	templateClonedFromRef := s.Blueprint.ClusterClass.Spec.ControlPlane.Ref
	cluster := s.Current.Cluster
	currentRef := cluster.Spec.ControlPlaneRef

	controlPlane, err := templateToObject(templateToInput{
		template:              template,
		templateClonedFromRef: templateClonedFromRef,
		cluster:               cluster,
		namePrefix:            fmt.Sprintf("%s-", cluster.Name),
		currentObjectRef:      currentRef,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate the ControlPlane object from the %s", template.GetKind())
	}

	// If the ClusterClass mandates the controlPlane has infrastructureMachines, add a reference to InfrastructureMachine
	// template and metadata to be used for the control plane machines.
	if s.Blueprint.HasControlPlaneInfrastructureMachine() {
		if err := contract.ControlPlane().MachineTemplate().InfrastructureRef().Set(controlPlane, infrastructureMachineTemplate); err != nil {
			return nil, errors.Wrap(err, "failed to spec.machineTemplate.infrastructureRef in the ControlPlane object")
		}

		// Compute the labels and annotations to be applied to ControlPlane machines.
		// We merge the labels and annotations from topology and ClusterClass.
		// We also add the cluster-name and the topology owned labels, so they are propagated down to Machines.
		topologyMetadata := s.Blueprint.Topology.ControlPlane.Metadata
		clusterClassMetadata := s.Blueprint.ClusterClass.Spec.ControlPlane.Metadata

		machineLabels := mergeMap(topologyMetadata.Labels, clusterClassMetadata.Labels)
		machineLabels[clusterv1.ClusterLabelName] = cluster.Name
		machineLabels[clusterv1.ClusterTopologyOwnedLabel] = ""
		if err := contract.ControlPlane().MachineTemplate().Metadata().Set(controlPlane,
			&clusterv1.ObjectMeta{
				Labels:      machineLabels,
				Annotations: mergeMap(topologyMetadata.Annotations, clusterClassMetadata.Annotations),
			}); err != nil {
			return nil, errors.Wrap(err, "failed to spec.machineTemplate.metadata in the ControlPlane object")
		}
	}

	// If it is required to manage the number of replicas for the control plane, set the corresponding field.
	// NOTE: If the Topology.ControlPlane.replicas value is nil, it is assumed that the control plane controller
	// does not implement support for this field and the ControlPlane object is generated without the number of Replicas.
	if s.Blueprint.Topology.ControlPlane.Replicas != nil {
		if err := contract.ControlPlane().Replicas().Set(controlPlane, int64(*s.Blueprint.Topology.ControlPlane.Replicas)); err != nil {
			return nil, errors.Wrap(err, "failed to set spec.replicas in the ControlPlane object")
		}
	}

	// Sets the desired Kubernetes version for the control plane.
	// TODO: improve this logic by adding support for version upgrade component by component
	if err := contract.ControlPlane().Version().Set(controlPlane, s.Blueprint.Topology.Version); err != nil {
		return nil, errors.Wrap(err, "failed to set spec.version in the ControlPlane object")
	}

	return controlPlane, nil
}

// computeCluster computes the desired state for the Cluster object.
// NOTE: Some fields of the Cluster’s fields contribute to defining the Cluster blueprint (e.g. Cluster.Spec.Topology),
// while some other fields should be managed as part of the actual Cluster (e.g. Cluster.Spec.ControlPlaneRef); in this func
// we are concerned only about the latest group of fields.
func computeCluster(_ context.Context, s *scope.Scope, infrastructureCluster, controlPlane *unstructured.Unstructured) *clusterv1.Cluster {
	cluster := s.Current.Cluster.DeepCopy()

	// Enforce the topology labels.
	// NOTE: The cluster label is added at creation time so this object could be read by the ClusterTopology
	// controller immediately after creation, even before other controllers are going to add the label (if missing).
	if cluster.Labels == nil {
		cluster.Labels = map[string]string{}
	}
	cluster.Labels[clusterv1.ClusterLabelName] = cluster.Name
	cluster.Labels[clusterv1.ClusterTopologyOwnedLabel] = ""

	// Set the references to the infrastructureCluster and controlPlane objects.
	// NOTE: Once set for the first time, the references are not expected to change.
	cluster.Spec.InfrastructureRef = contract.ObjToRef(infrastructureCluster)
	cluster.Spec.ControlPlaneRef = contract.ObjToRef(controlPlane)

	return cluster
}

// computeMachineDeployment computes the desired state for a MachineDeploymentTopology.
// The generated machineDeployment object is calculated using the values from the machineDeploymentTopology and
// the machineDeployment class.
func computeMachineDeployment(_ context.Context, s *scope.Scope, machineDeploymentTopology clusterv1.MachineDeploymentTopology) (*scope.MachineDeploymentState, error) {
	desiredMachineDeployment := &scope.MachineDeploymentState{}

	// Gets the blueprint for the MachineDeployment class.
	className := machineDeploymentTopology.Class
	machineDeploymentBlueprint, ok := s.Blueprint.MachineDeployments[className]
	if !ok {
		return nil, errors.Errorf("MachineDeployment blueprint %s not found in ClusterClass %s", className, s.Blueprint.ClusterClass.Name)
	}

	// Compute the boostrap template.
	currentMachineDeployment := s.Current.MachineDeployments[machineDeploymentTopology.Name]
	var currentBootstrapTemplateRef *corev1.ObjectReference
	if currentMachineDeployment != nil && currentMachineDeployment.BootstrapTemplate != nil {
		currentBootstrapTemplateRef = currentMachineDeployment.Object.Spec.Template.Spec.Bootstrap.ConfigRef
	}
	desiredMachineDeployment.BootstrapTemplate = templateToTemplate(templateToInput{
		template:              machineDeploymentBlueprint.BootstrapTemplate,
		templateClonedFromRef: contract.ObjToRef(machineDeploymentBlueprint.BootstrapTemplate),
		cluster:               s.Current.Cluster,
		namePrefix:            bootstrapTemplateNamePrefix(s.Current.Cluster.Name, machineDeploymentTopology.Name),
		currentObjectRef:      currentBootstrapTemplateRef,
	})

	bootstrapTemplateLabels := desiredMachineDeployment.BootstrapTemplate.GetLabels()
	if bootstrapTemplateLabels == nil {
		bootstrapTemplateLabels = map[string]string{}
	}
	// Add ClusterTopologyMachineDeploymentLabel to the generated Bootstrap template
	bootstrapTemplateLabels[clusterv1.ClusterTopologyMachineDeploymentLabelName] = machineDeploymentTopology.Name
	desiredMachineDeployment.BootstrapTemplate.SetLabels(bootstrapTemplateLabels)

	// Compute the Infrastructure template.
	var currentInfraMachineTemplateRef *corev1.ObjectReference
	if currentMachineDeployment != nil && currentMachineDeployment.InfrastructureMachineTemplate != nil {
		currentInfraMachineTemplateRef = &currentMachineDeployment.Object.Spec.Template.Spec.InfrastructureRef
	}
	desiredMachineDeployment.InfrastructureMachineTemplate = templateToTemplate(templateToInput{
		template:              machineDeploymentBlueprint.InfrastructureMachineTemplate,
		templateClonedFromRef: contract.ObjToRef(machineDeploymentBlueprint.InfrastructureMachineTemplate),
		cluster:               s.Current.Cluster,
		namePrefix:            infrastructureMachineTemplateNamePrefix(s.Current.Cluster.Name, machineDeploymentTopology.Name),
		currentObjectRef:      currentInfraMachineTemplateRef,
	})

	infraMachineTemplateLabels := desiredMachineDeployment.InfrastructureMachineTemplate.GetLabels()
	if infraMachineTemplateLabels == nil {
		infraMachineTemplateLabels = map[string]string{}
	}
	// Add ClusterTopologyMachineDeploymentLabel to the generated InfrastructureMachine template
	infraMachineTemplateLabels[clusterv1.ClusterTopologyMachineDeploymentLabelName] = machineDeploymentTopology.Name
	desiredMachineDeployment.InfrastructureMachineTemplate.SetLabels(infraMachineTemplateLabels)

	// Compute the MachineDeployment object.
	gv := clusterv1.GroupVersion
	desiredMachineDeploymentObj := &clusterv1.MachineDeployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       gv.WithKind("MachineDeployment").Kind,
			APIVersion: gv.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName(fmt.Sprintf("%s-%s-", s.Current.Cluster.Name, machineDeploymentTopology.Name)),
			Namespace: s.Current.Cluster.Namespace,
		},
		Spec: clusterv1.MachineDeploymentSpec{
			ClusterName: s.Current.Cluster.Name,
			Template: clusterv1.MachineTemplateSpec{
				ObjectMeta: clusterv1.ObjectMeta{
					Labels:      mergeMap(machineDeploymentTopology.Metadata.Labels, machineDeploymentBlueprint.Metadata.Labels),
					Annotations: mergeMap(machineDeploymentTopology.Metadata.Annotations, machineDeploymentBlueprint.Metadata.Annotations),
				},
				Spec: clusterv1.MachineSpec{
					ClusterName: s.Current.Cluster.Name,
					// Sets the desired Kubernetes version for the MachineDeployment.
					// TODO: improve this logic by adding support for version upgrade component by component
					Version:           pointer.String(s.Blueprint.Topology.Version),
					Bootstrap:         clusterv1.Bootstrap{ConfigRef: contract.ObjToRef(desiredMachineDeployment.BootstrapTemplate)},
					InfrastructureRef: *contract.ObjToRef(desiredMachineDeployment.InfrastructureMachineTemplate),
				},
			},
		},
	}

	// If an existing MachineDeployment is present, override the MachineDeployment generate name
	// re-using the existing name (this will help in reconcile).
	if currentMachineDeployment != nil && currentMachineDeployment.Object != nil {
		desiredMachineDeploymentObj.SetName(currentMachineDeployment.Object.Name)
	}

	// Apply Labels
	// NOTE: On top of all the labels applied to managed objects we are applying the ClusterTopologyMachineDeploymentLabel
	// keeping track of the MachineDeployment name from the Topology; this will be used to identify the object in next reconcile loops.
	labels := map[string]string{}
	labels[clusterv1.ClusterLabelName] = s.Current.Cluster.Name
	labels[clusterv1.ClusterTopologyOwnedLabel] = ""
	labels[clusterv1.ClusterTopologyMachineDeploymentLabelName] = machineDeploymentTopology.Name
	desiredMachineDeploymentObj.SetLabels(labels)

	// Also set the labels in .spec.template.labels so that they are propagated to
	// MachineSet.labels and MachineSet.spec.template.labels and thus to Machine.labels.
	// Note: the labels in MachineSet are used to properly cleanup templates when the MachineSet is deleted.
	desiredMachineDeploymentObj.Spec.Template.Labels[clusterv1.ClusterLabelName] = s.Current.Cluster.Name
	desiredMachineDeploymentObj.Spec.Template.Labels[clusterv1.ClusterTopologyOwnedLabel] = ""
	desiredMachineDeploymentObj.Spec.Template.Labels[clusterv1.ClusterTopologyMachineDeploymentLabelName] = machineDeploymentTopology.Name

	// Set the desired replicas.
	desiredMachineDeploymentObj.Spec.Replicas = machineDeploymentTopology.Replicas

	desiredMachineDeployment.Object = desiredMachineDeploymentObj
	return desiredMachineDeployment, nil
}

type templateToInput struct {
	template              *unstructured.Unstructured
	templateClonedFromRef *corev1.ObjectReference
	cluster               *clusterv1.Cluster
	namePrefix            string
	currentObjectRef      *corev1.ObjectReference
}

// templateToObject generates an object from a template, taking care
// of adding required labels (cluster, topology), annotations (clonedFrom)
// and assigning a meaningful name (or reusing current reference name).
func templateToObject(in templateToInput) (*unstructured.Unstructured, error) {
	// NOTE: The cluster label is added at creation time so this object could be read by the ClusterTopology
	// controller immediately after creation, even before other controllers are going to add the label (if missing).
	labels := map[string]string{}
	labels[clusterv1.ClusterLabelName] = in.cluster.Name
	labels[clusterv1.ClusterTopologyOwnedLabel] = ""

	// Generate the object from the template.
	// NOTE: OwnerRef can't be set at this stage; other controllers are going to add OwnerReferences when
	// the object is actually created.
	object, err := external.GenerateTemplate(&external.GenerateTemplateInput{
		Template:    in.template,
		TemplateRef: in.templateClonedFromRef,
		Namespace:   in.cluster.Namespace,
		Labels:      labels,
		ClusterName: in.cluster.Name,
	})
	if err != nil {
		return nil, err
	}

	// Ensure the generated objects have a meaningful name.
	// NOTE: In case there is already a ref to this object in the Cluster, re-use the same name
	// in order to simplify compare at later stages of the reconcile process.
	object.SetName(names.SimpleNameGenerator.GenerateName(in.namePrefix))
	if in.currentObjectRef != nil && len(in.currentObjectRef.Name) > 0 {
		object.SetName(in.currentObjectRef.Name)
	}

	return object, nil
}

// templateToTemplate generates a template from an existing template, taking care
// of adding required labels (cluster, topology), annotations (clonedFrom)
// and assigning a meaningful name (or reusing current reference name).
// NOTE: We are creating a copy of the ClusterClass template for each cluster so
// it is possible to add cluster specific information without affecting the original object.
func templateToTemplate(in templateToInput) *unstructured.Unstructured {
	template := &unstructured.Unstructured{}
	in.template.DeepCopyInto(template)

	// Remove all the info automatically assigned by the API server and not relevant from
	// the copy of the template.
	template.SetResourceVersion("")
	template.SetFinalizers(nil)
	template.SetUID("")
	template.SetSelfLink("")

	// Enforce the topology labels into the provided label set.
	// NOTE: The cluster label is added at creation time so this object could be read by the ClusterTopology
	// controller immediately after creation, even before other controllers are going to add the label (if missing).
	labels := template.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[clusterv1.ClusterLabelName] = in.cluster.Name
	labels[clusterv1.ClusterTopologyOwnedLabel] = ""
	template.SetLabels(labels)

	// Enforce cloned from annotations.
	annotations := template.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[clusterv1.TemplateClonedFromNameAnnotation] = in.templateClonedFromRef.Name
	annotations[clusterv1.TemplateClonedFromGroupKindAnnotation] = in.templateClonedFromRef.GroupVersionKind().GroupKind().String()
	template.SetAnnotations(annotations)

	// Ensure the generated template gets a meaningful name.
	// NOTE: In case there is already an object ref to this template, it is required to re-use the same name
	// in order to simplify compare at later stages of the reconcile process.
	template.SetName(names.SimpleNameGenerator.GenerateName(in.namePrefix))
	if in.currentObjectRef != nil && len(in.currentObjectRef.Name) > 0 {
		template.SetName(in.currentObjectRef.Name)
	}

	return template
}

// mergeMap merges two maps into another one.
// NOTE: In case a key exists in both maps, the value in the first map is preserved.
func mergeMap(a, b map[string]string) map[string]string {
	m := make(map[string]string)
	for k, v := range b {
		m[k] = v
	}
	for k, v := range a {
		m[k] = v
	}
	return m
}
