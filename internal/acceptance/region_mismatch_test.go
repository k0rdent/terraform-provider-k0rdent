package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// v1.3.0-dev.4 locks a project to one region, and refuses every
// project-scoped request in another with a 409 PROJECT_REGION_MISMATCH that
// doesn't name the project's region. The seeded project `demo` is in
// us-texas-1.

// A read the platform refuses as a region mismatch says which region the
// project is in and what to set.
func TestAccARegionMismatchNamesTheProjectsRegion(t *testing.T) {
	t.Parallel()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
data %q "elsewhere" {
  region  = "eu-west-1"
  project = "demo"
}
`, networksType),
			ExpectError: regexp.MustCompile(`(?s)PROJECT_REGION_MISMATCH.*"demo" is in region "us-texas-1".*set\s+` + "`region`"),
		}},
	})
}
