package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// refusal is the platform's error envelope for status, with reason at the
// top level or, when inDetail is set, in a detail.
func refusal(t *testing.T, status int, reason, message string, inDetail bool) (*http.Response, []byte) {
	t.Helper()
	e := map[string]any{"code": status, "message": message, "requestId": "req-guide"}
	if inDetail {
		e["details"] = []any{map[string]any{"reason": reason}}
	} else if reason != "" {
		e["reason"] = reason
	}
	body, err := json.Marshal(map[string]any{"error": e})
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: http.Header{"Content-Type": []string{"application/json"}}}
	return resp, body
}

// Every known reason gets its next step, after the platform's message and
// before the request id, wherever the reason rides.
func TestEveryKnownReasonSaysWhatToDo(t *testing.T) {
	tests := []struct {
		reason, message string
		status          int
		inDetail        bool
		want            string
	}{
		// The four seen on a deployed platform, with their text as the platform sent it.
		{"NON_INTERACTIVE_PRINCIPAL", "non-interactive principals cannot create credentials or modify access policies",
			403, false, "as a person in the console"},
		{"INSUFFICIENT_CAPACITY", "no free block of size /24 in pool external", 409, true, "larger `prefix_length`"},
		{"INSUFFICIENT_CAPACITY", "no free block of size /24 in pool external", 422, false, "non-routable pool"},
		{"", "VPCPrefix.spec.prefixLength: prefixLength must be at most 31", 400, false,
			"VPCPrefix is an object the platform builds for itself"},

		{"CONFLICT_BACKEND_CAPACITY_EXCEEDED", "no sellable capacity", 409, false, "another `share_type`"},
		{"CONFLICT_CAPACITY_BELOW_USAGE", "capacity below usage", 409, true, "at or above the store's usage"},
		{"CONFLICT_IMMUTABLE", "field is immutable", 409, true, "terraform apply -replace"},
		{"QUOTA_EXCEEDED", "quota exceeded", 409, true, "have the binding limit raised"},
		{"QUOTA_UNASSIGNED", "no quota profile", 409, false, "An operator has to assign it"},
		{"PROJECT_REGION_MISMATCH", "project is in another region", 409, false, "Set `region` to the project's region"},
		{"", "upstream unavailable", 503, false, "The platform failed, not the configuration"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %d", tt.reason, tt.status), func(t *testing.T) {
			resp, body := refusal(t, tt.status, tt.reason, tt.message, tt.inDetail)
			var diags diag.Diagnostics
			addAPIError(&diags, "create the subnet", resp, body, nil)
			detail := diags.Errors()[0].Detail()
			at := strings.Index(detail, tt.want)
			if at < 0 {
				t.Fatalf("detail does not say %q:\n%s", tt.want, detail)
			}
			if m := strings.Index(detail, tt.message); m < 0 || m > at {
				t.Errorf("the platform's message isn't kept ahead of the next step:\n%s", detail)
			}
			if id := strings.Index(detail, "Request id: req-guide"); id < at {
				t.Errorf("the request id isn't kept after the next step:\n%s", detail)
			}
			next := guidanceFor(k0rdent.NewAPIError(resp, body))
			for _, banned := range []string{"\u2014", ";", "->", "\u2192", "customer"} {
				if strings.Contains(next, banned) {
					t.Errorf("the next step contains %q: %s", banned, next)
				}
			}
		})
	}
}

// Each guidance entry is tested above, so none is dead text.
func TestEveryGuidanceEntryIsTested(t *testing.T) {
	tested := map[string]bool{"NON_INTERACTIVE_PRINCIPAL": true, "INSUFFICIENT_CAPACITY": true,
		"CONFLICT_BACKEND_CAPACITY_EXCEEDED": true, "CONFLICT_CAPACITY_BELOW_USAGE": true,
		"CONFLICT_IMMUTABLE": true, "QUOTA_EXCEEDED": true, "QUOTA_UNASSIGNED": true}
	for reason := range reasonGuidance {
		if !tested[reason] {
			t.Errorf("reason %s has guidance and no case in TestEveryKnownReasonSaysWhatToDo", reason)
		}
	}
}

// A bare 403 on a read or a project write only an organization role can make
// says which role it needs, and that a credential scoped to a project can't
// have it. The same
// 403 on another operation, or with a reason, isn't given that guidance.
func TestAForbiddenOrganizationReadSaysWhichRoleItNeeds(t *testing.T) {
	forbidden := func(operation, reason string) *k0rdent.APIError {
		resp, body := refusal(t, http.StatusForbidden, reason, "forbidden", false)
		apiErr := k0rdent.NewAPIError(resp, body)
		apiErr.Operation = operation
		return apiErr
	}
	tests := []struct {
		operation string
		want      []string
	}{
		{"getOrganization", []string{"organizations-viewer", "scoped to a project",
			"`" + ResourceName(organizationsDataSourceSuffix) + "`"}},
		{"getProjectInOrg", []string{"organizations-viewer or compute-viewer", "A role on the project doesn't count",
			"`" + ResourceName(projectDataSourceSuffix) + "` data source"}},
		{"getProjectQuota", []string{"Reading quota", "organizations-viewer", "scoped to a project"}},
		{"getTenantQuota", []string{"Reading quota", "organizations-viewer", "scoped to a project"}},
		{"listRoles", []string{"iam-viewer or iam-admin", "scoped to a project"}},
		{"getServiceAccount", []string{"iam-admin", "not iam-viewer", "scoped to a project"}},
		{"listServiceAccounts", []string{"iam-admin", "not iam-viewer", "scoped to a project"}},
		{"listServiceAccountCredentials", []string{"iam-admin", "not iam-viewer", "scoped to a project"}},
		// A destroy has to delete the project too, so a create names the
		// role a delete needs.
		{"createProjectInOrg", []string{"Creating a project needs organizations-member or organizations-admin",
			"Only organizations-admin can delete it", "scoped to the organization"}},
		{"deleteProjectInOrg", []string{"Deleting a project needs organizations-admin", "not organizations-member",
			"scoped to a project"}},
	}
	tested := map[string]bool{}
	for _, tt := range tests {
		tested[tt.operation] = true
		t.Run(tt.operation, func(t *testing.T) {
			apiErr := forbidden(tt.operation, "")
			next := guidanceFor(apiErr)
			// The gateway's 403 for an organization that doesn't exist is
			// the same answer, so a wrong id is named alongside the role.
			for _, want := range append(tt.want, "same 403 for an organization id that doesn't exist") {
				if !strings.Contains(next, want) {
					t.Errorf("the next step doesn't say %q: %s", want, next)
				}
			}
			for _, banned := range []string{"—", ";", "->", "→", "customer"} {
				if strings.Contains(next, banned) {
					t.Errorf("the next step contains %q: %s", banned, next)
				}
			}
			if n := strings.Count(next, ". "); n > 1 {
				t.Errorf("the next step is %d sentences, want one or two: %s", n+1, next)
			}
			detail := apiErrorDetail(apiErr)
			if at, id := strings.Index(detail, next), strings.Index(detail, "Request id: req-guide"); at < 0 || id < at {
				t.Errorf("the next step isn't between the platform's answer and the request id:\n%s", detail)
			}
			if got := guidanceFor(forbidden(tt.operation, "NON_INTERACTIVE_PRINCIPAL")); got != reasonGuidance["NON_INTERACTIVE_PRINCIPAL"] {
				t.Errorf("a 403 with a known reason got %q", got)
			}
			// A reason nobody has seen is some other refusal, not the role.
			if got := guidanceFor(forbidden(tt.operation, "SOMETHING_NEW")); got != "" {
				t.Errorf("a 403 with an unknown reason got %q", got)
			}
		})
	}
	for op := range forbiddenGuidance {
		if !tested[op] {
			t.Errorf("operation %s has 403 guidance and no case here", op)
		}
	}
	for _, other := range []string{"listClusters", ""} {
		if got := guidanceFor(forbidden(other, "")); got != "" {
			t.Errorf("a 403 on %q got guidance: %s", other, got)
		}
	}
}

// A reason the provider doesn't know still shows the status, the message,
// the reason and the request id, and adds no next step it can't back.
func TestAnUnknownReasonStillShowsWhatThePlatformSaid(t *testing.T) {
	resp, body := refusal(t, 409, "SOMETHING_NEW", "a refusal nobody has seen", false)
	var diags diag.Diagnostics
	addAPIError(&diags, "create the subnet", resp, body, nil)
	detail := diags.Errors()[0].Detail()
	want := "The platform answered 409 Conflict.\n\na refusal nobody has seen\n\nReason: SOMETHING_NEW\n\nRequest id: req-guide"
	if detail != want {
		t.Errorf("detail:\n%s\nwant:\n%s", detail, want)
	}
}

// A caller with advice of its own for the operation gives one next step,
// its own.
func TestAdviceOfTheCallersOwnReplacesTheGuidance(t *testing.T) {
	resp, body := refusal(t, 409, "QUOTA_EXCEEDED", "quota exceeded", true)
	apiErr := k0rdent.NewAPIError(resp, body)
	err := fmt.Errorf("scale: %w", error(apiErr))
	var diags diag.Diagnostics
	addAdvisedError(&diags, "Could not scale", "Grow the pools by less.", err)
	detail := diags.Errors()[0].Detail()
	if strings.Contains(detail, reasonGuidance["QUOTA_EXCEEDED"]) || !strings.Contains(detail, "Grow the pools by less.") {
		t.Errorf("detail:\n%s", detail)
	}

	diags = nil
	addAdvisedError(&diags, "Could not scale", "", err)
	if !strings.Contains(diags.Errors()[0].Detail(), reasonGuidance["QUOTA_EXCEEDED"]) {
		t.Errorf("no advice of its own, and no guidance:\n%s", diags.Errors()[0].Detail())
	}
}
