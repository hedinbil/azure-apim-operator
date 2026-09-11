/*
Copyright 2025.

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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	apimv1 "github.com/hedinit/azure-apim-operator/api/v1"
)

func TestSpecOrDeletionChanged(t *testing.T) {
	p := specOrDeletionChanged()

	base := &apimv1.APIMProduct{ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 1}}
	statusOnly := base.DeepCopy()
	statusOnly.Status.Phase = "Error"
	specChanged := base.DeepCopy()
	specChanged.Generation = 2
	now := metav1.Now()
	deleting := base.DeepCopy()
	deleting.DeletionTimestamp = &now

	if !p.Create(event.CreateEvent{Object: base}) {
		t.Error("create must reconcile")
	}
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: statusOnly}) {
		t.Error("a status-only update must not reconcile")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: specChanged}) {
		t.Error("a spec change (generation bump) must reconcile")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: deleting}) {
		t.Error("the start of a deletion must reconcile")
	}
}
