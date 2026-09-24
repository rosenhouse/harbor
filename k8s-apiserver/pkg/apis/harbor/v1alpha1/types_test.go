package v1alpha1_test

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse/harbor/k8s-apiserver/pkg/apis/harbor/v1alpha1"
)

func TestHarborReplicationJSON(t *testing.T) {
	start := metav1.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	end := metav1.Date(2026, 9, 23, 10, 0, 30, 0, time.UTC)
	for _, tc := range []struct {
		name string
		obj  v1alpha1.HarborReplication
		want string
	}{
		{
			name: "minimal",
			obj: v1alpha1.HarborReplication{
				Spec: v1alpha1.HarborReplicationSpec{Registry: "docker-hub", Repository: "library/nginx", Tag: "*"},
			},
			want: `{
  "metadata": {},
  "spec": {
    "registry": "docker-hub",
    "repository": "library/nginx",
    "tag": "*"
  },
  "status": {}
}`,
		},
		{
			name: "full",
			obj: v1alpha1.HarborReplication{
				Spec: v1alpha1.HarborReplicationSpec{Registry: "docker-hub", Repository: "library/nginx", Tag: "1.27*", Schedule: "0 0 3 * * *"},
				Status: v1alpha1.HarborReplicationStatus{
					Destination: "my-project/k8s/team-a/nginx",
					LastExecution: &v1alpha1.HarborReplicationExecution{
						ID:         42,
						Trigger:    v1alpha1.ReplicationTriggerScheduled,
						Phase:      v1alpha1.ReplicationPhaseFailed,
						Message:    "1 of 5 tasks failed",
						StartTime:  &start,
						EndTime:    &end,
						Total:      5,
						Succeeded:  1,
						Failed:     2,
						InProgress: 3,
						Stopped:    4,
					},
				},
			},
			want: `{
  "metadata": {},
  "spec": {
    "registry": "docker-hub",
    "repository": "library/nginx",
    "tag": "1.27*",
    "schedule": "0 0 3 * * *"
  },
  "status": {
    "destination": "my-project/k8s/team-a/nginx",
    "lastExecution": {
      "id": 42,
      "trigger": "Scheduled",
      "phase": "Failed",
      "message": "1 of 5 tasks failed",
      "startTime": "2026-09-23T10:00:00Z",
      "endTime": "2026-09-23T10:00:30Z",
      "total": 5,
      "succeeded": 1,
      "failed": 2,
      "inProgress": 3,
      "stopped": 4
    }
  }
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tc.obj, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}
