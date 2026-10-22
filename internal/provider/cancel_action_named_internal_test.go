package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// Every place that tells a reader how a cluster's operation gets cancelled
// names the cancel action, since it's the one way that keeps the cluster, and
// none of them says an apply never cancels: the destroy half of an apply that
// replaces a busy cluster, a tainted one included, cancels its operation.
func TestEveryCancelExplanationNamesTheAction(t *testing.T) {
	ctx := context.Background()
	name := ResourceName(clusterCancelOperationActionSuffix)

	var cluster resource.SchemaResponse
	(&clusterResource{}).Schema(ctx, resource.SchemaRequest{}, &cluster)
	var act action.SchemaResponse
	(&clusterCancelOperationAction{}).Schema(ctx, action.SchemaRequest{}, &act)

	for _, tc := range []struct{ where, text string }{
		{"the cluster's description", cluster.Schema.MarkdownDescription},
		{"a destroy's wait that timed out", awaitTimedOutAdvice("updating")},
		{"a change's wait that timed out", changeKeepsRunning},
	} {
		if !strings.Contains(tc.text, name) {
			t.Errorf("%s doesn't name %s:\n%s", tc.where, name, tc.text)
		}
	}
	for _, tc := range []struct{ where, text string }{
		{"the cluster's description", cluster.Schema.MarkdownDescription},
		{"the action's description", act.Schema.MarkdownDescription},
	} {
		for _, claim := range []string{"No plan or apply cancels", "Nothing but a destroy cancels"} {
			if strings.Contains(tc.text, claim) {
				t.Errorf("%s says %q, and an apply replacing a busy cluster cancels its operation", tc.where, claim)
			}
		}
	}
}
