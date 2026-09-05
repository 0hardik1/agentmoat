// The gpu-passthrough evidence must name the resource the container really
// asked for: an operator reading "nvidia.com/mig-1g.5gb" learns in one
// glance why the verdict is error (nvproxy has no MIG support).
package explainer

import (
	"reflect"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestEvidenceForGPUPassthrough_ActualResourceNames(t *testing.T) {
	w := scanner.Workload{PodSpec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "trainer", Resources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2"), "cpu": resource.MustParse("1")},
			Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")},
		}},
		{Name: "slicer", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/mig-1g.5gb": resource.MustParse("1"), "nvidia.com/gpu.shared": resource.MustParse("3")},
		}},
		{Name: "cpu-only"},
	}}}
	got := evidenceForGPUPassthrough(w).GPURequests
	want := []schema.GPURequest{
		{Container: "trainer", Resource: "nvidia.com/gpu", Quantity: "2"},
		{Container: "slicer", Resource: "nvidia.com/gpu.shared", Quantity: "3"},
		{Container: "slicer", Resource: "nvidia.com/mig-1g.5gb", Quantity: "1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GPURequests:\n got %+v\nwant %+v", got, want)
	}
}
