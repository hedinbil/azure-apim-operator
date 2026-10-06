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

// TestSpecOrDeletionChangedRetryAnnotation: setting apim.operator.io/retry is a
// metadata-only change with no generation bump, and must still reach Reconcile.
func TestSpecOrDeletionChangedRetryAnnotation(t *testing.T) {
	p := specOrDeletionChanged()

	base := &apimv1.APIMTag{ObjectMeta: metav1.ObjectMeta{Name: "t", Generation: 3}}
	withRetry := func(value string) *apimv1.APIMTag {
		obj := base.DeepCopy()
		obj.Annotations = map[string]string{retryAnnotation: value}
		return obj
	}
	otherAnnotation := base.DeepCopy()
	otherAnnotation.Annotations = map[string]string{"example.com/note": "x"}

	cases := []struct {
		name     string
		old, new *apimv1.APIMTag
		want     bool
	}{
		{"annotation added", base, withRetry("1"), true},
		{"annotation value changed", withRetry("1"), withRetry("2"), true},
		{"annotation unchanged", withRetry("1"), withRetry("1"), false},
		{"annotation removed", withRetry("1"), base, true},
		{"another annotation", base, otherAnnotation, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
				t.Errorf("Update() = %v, want %v", got, tc.want)
			}
		})
	}
	if retryAnnotationChanged(event.UpdateEvent{ObjectNew: base}) {
		t.Error("a nil old object must not count as a change")
	}
}
