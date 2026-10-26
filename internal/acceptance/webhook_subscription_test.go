package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	webhookType               = provider.ResourceName("webhook_subscription")
	webhookAddr               = webhookType + ".test"
	webhookDataType           = provider.ResourceName("webhook_subscription")
	webhooksDataType          = provider.ResourceName("webhook_subscriptions")
	webhookDeliveriesDataType = provider.ResourceName("webhook_deliveries")
)

// webhookSecretPrefix starts every signing secret this suite configures. Gate
// S4 searches the trace log and the fake's log for it, and every test here
// searches the state for the whole secret, so a secret that reached either is
// a failure rather than something nobody looked for.
const webhookSecretPrefix = "acc-whsec-"

// webhookSecret is a signing secret of the length the contract asks for, at
// least 32 characters, that names the step it belongs to.
func webhookSecret(label string) string {
	s := webhookSecretPrefix + label
	for len(s) < 40 {
		s += "-0123456789"
	}
	return s
}

// webhookURL is an endpoint whose host starts with the sweep prefix, which is
// how the sweeper knows a subscription is this suite's: the platform picks
// the id, so the id can't say.
func webhookURL(name string) string {
	return "https://acc-hooks.example.test/" + name
}

// webhookConfig is the resource at <type>.test. A nil eventTypes leaves the
// filter out. A version above 0 sets `secret_wo` from the ephemeral variable
// webhook_secret, which webhookVars fills in, and leaves the secret to the
// platform at 0.
//
// The secret goes through a variable rather than a literal because the test
// framework writes each step's configuration to the trace log, and gate S4
// would rightly find it there. It's also how a configuration should hand one
// over: an ephemeral variable, which Terraform never stores either.
func webhookConfig(url string, eventTypes []string, version int, extra string) string {
	var b strings.Builder
	if version > 0 {
		b.WriteString("variable \"webhook_secret\" {\n  type      = string\n  sensitive = true\n  ephemeral = true\n}\n\n")
	}
	fmt.Fprintf(&b, "resource %q \"test\" {\n  url = %q\n", webhookType, url)
	if eventTypes != nil {
		quoted := make([]string, len(eventTypes))
		for i, e := range eventTypes {
			quoted[i] = fmt.Sprintf("%q", e)
		}
		fmt.Fprintf(&b, "  event_types = [%s]\n", strings.Join(quoted, ", "))
	}
	if version > 0 {
		fmt.Fprintf(&b, "  secret_wo         = var.webhook_secret\n  secret_wo_version = %d\n", version)
	}
	b.WriteString(extra)
	b.WriteString("}\n")
	return b.String()
}

// webhookConfigFixedSecret is the resource at <type>.test under a secret
// derived from seed in the configuration itself, at version 1, for a case that
// takes a bare configuration and no variables. The derived value is never
// written down anywhere a log keeps, and seed isn't a secret.
func webhookConfigFixedSecret(url string, eventTypes []string, seed string) string {
	config := strings.Replace(webhookConfig(url, eventTypes, 1, ""), "var.webhook_secret", fmt.Sprintf("sha256(%q)", seed), 1)
	return config[strings.Index(config, "resource "):]
}

// webhookVars hands a step its signing secret, or nothing for "".
func webhookVars(secret string) config.Variables {
	if secret == "" {
		return nil
	}
	return config.Variables{"webhook_secret": config.StringVariable(secret)}
}

// webhookPath is the fake store's row for a subscription. The API has no read
// of one subscription, so this path exists only behind the harness's owner
// token, where the store answers it.
func webhookPath(a map[string]string) string {
	return "/v1/regions/global/notifications/webhooks/" + a["id"]
}

// webhookRow is what the fake's store holds for a subscription, signing
// secrets included. The API never answers either secret after the call that
// set it, which is why the provider can't check them and this can.
type webhookRow struct {
	ID             string   `json:"id"`
	URL            string   `json:"url"`
	EventTypes     []string `json:"event_types"`
	SigningSecret  string   `json:"signing_secret"`
	PreviousSecret string   `json:"previous_secret"`
	SecretVersion  int      `json:"secret_version"`
}

func readWebhookRow(id string) (webhookRow, error) {
	var row webhookRow
	status, body, err := readFakeStore(webhookPath(map[string]string{"id": id}))
	if err != nil {
		return row, err
	}
	if status != http.StatusOK {
		return row, fmt.Errorf("the fake's store answered %d for subscription %s: %s", status, id, body)
	}
	return row, json.Unmarshal(body, &row)
}

// checkWebhookRow reads the subscription the state names from the fake's
// store and hands it to check.
func checkWebhookRow(check func(webhookRow) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[webhookAddr]
		if !ok {
			return fmt.Errorf("%s is not in state", webhookAddr)
		}
		row, err := readWebhookRow(rs.Primary.ID)
		if err != nil {
			return err
		}
		return check(row)
	}
}

// checkSigning asserts the secret the platform signs with now, and the one it
// still accepts from before a rotation ("" for none).
func checkSigning(current, previous string) resource.TestCheckFunc {
	return checkWebhookRow(func(row webhookRow) error {
		if row.SigningSecret != current {
			return fmt.Errorf("the platform signs with a secret of %d characters that isn't the one "+
				"configured", len(row.SigningSecret))
		}
		if row.PreviousSecret != previous {
			return fmt.Errorf("the platform's previous secret is %d characters, want %d",
				len(row.PreviousSecret), len(previous))
		}
		return nil
	})
}

// checkNoSecretInState fails if any attribute of any resource in the state
// holds one of secrets. Write-only is the whole reason the secret can be
// configured at all (no secret in state), so it's checked against
// the state Terraform wrote rather than assumed from the schema.
func checkNoSecretInState(secrets ...string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for addr, rs := range s.RootModule().Resources {
			for name, v := range rs.Primary.Attributes {
				for _, secret := range secrets {
					if strings.Contains(v, secret) {
						return fmt.Errorf("%s.%s holds a signing secret", addr, name)
					}
				}
			}
		}
		return nil
	}
}

// checkWebhookGone is the CheckDestroy for a subscription.
var checkWebhookGone = checkGone(webhookType, webhookPath)

// The lifecycle, and the reason the resource exists in this shape: a new
// secret is a rotation of the same subscription, never a replacement.
//
// Rotation keeps the id, so the endpoint's delivery history and everything
// queued for it survive, which a delete and a create would lose. The overlap
// the platform keeps by default is what lets receivers move to the new secret
// at their own pace, so the previous secret has to still be accepted after a
// rotation, and gone after one asked to cut over at once.
func TestAccWebhookSubscription(t *testing.T) {
	url := webhookURL("lifecycle")
	events := []string{"io.k0rdent.cluster.ready.v1", "io.k0rdent.cluster.failed.v1"}
	first, second, third := webhookSecret("first"), webhookSecret("second"), webhookSecret("third")
	var id string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{
			{
				Config:          webhookConfig(url, events, 1, ""),
				ConfigVariables: webhookVars(first),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(webhookAddr, "id", uuidPattern),
					resource.TestCheckResourceAttr(webhookAddr, "url", url),
					resource.TestCheckResourceAttr(webhookAddr, "event_types.#", "2"),
					resource.TestCheckTypeSetElemAttr(webhookAddr, "event_types.*", events[0]),
					resource.TestCheckTypeSetElemAttr(webhookAddr, "event_types.*", events[1]),
					resource.TestCheckResourceAttr(webhookAddr, "secret_wo_version", "1"),
					resource.TestCheckNoResourceAttr(webhookAddr, "secret_wo"),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "1"),
					resource.TestCheckResourceAttr(webhookAddr, "is_active", "true"),
					resource.TestCheckResourceAttr(webhookAddr, "rotation_overlap", "true"),
					resource.TestCheckResourceAttrSet(webhookAddr, "created_at"),
					resource.TestCheckNoResourceAttr(webhookAddr, "rotated_at"),
					checkSigning(first, ""),
					checkNoSecretInState(first),
					func(s *terraform.State) error {
						id = s.RootModule().Resources[webhookAddr].Primary.ID
						return nil
					},
				),
			},
			{Config: webhookConfig(url, events, 1, ""), ConfigVariables: webhookVars(first), PlanOnly: true},
			{
				// A new secret, and the version bumped to say so. Terraform
				// never stores the secret, so the version is the only change
				// a plan can see.
				Config:          webhookConfig(url, events, 2, ""),
				ConfigVariables: webhookVars(second),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(webhookAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(webhookAddr, "id", sameAs(&id)),
					resource.TestCheckResourceAttr(webhookAddr, "secret_wo_version", "2"),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "2"),
					resource.TestCheckResourceAttrSet(webhookAddr, "rotated_at"),
					resource.TestCheckResourceAttrSet(webhookAddr, "previous_secret_expires_at"),
					checkSigning(second, first),
					checkNoSecretInState(first, second),
				),
			},
			{Config: webhookConfig(url, events, 2, ""), ConfigVariables: webhookVars(second), PlanOnly: true},
			{
				// The cutover a leaked secret calls for: the previous one is
				// dropped rather than kept for the overlap window.
				Config:          webhookConfig(url, events, 3, "  rotation_overlap = false\n"),
				ConfigVariables: webhookVars(third),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(webhookAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(webhookAddr, "id", sameAs(&id)),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "3"),
					resource.TestCheckResourceAttr(webhookAddr, "rotation_overlap", "false"),
					resource.TestCheckNoResourceAttr(webhookAddr, "previous_secret_expires_at"),
					checkSigning(third, ""),
					checkNoSecretInState(first, second, third),
				),
			},
			{
				// Turning the overlap back on is a setting for the next
				// rotation. It sends nothing, so the secret stays put.
				Config:          webhookConfig(url, events, 3, ""),
				ConfigVariables: webhookVars(third),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(webhookAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "3"),
					checkSigning(third, ""),
				),
			},
			{
				// There's no update for the filter, so a change to it is a new
				// subscription, under the secret the configuration holds.
				Config:          webhookConfig(url, events[:1], 3, ""),
				ConfigVariables: webhookVars(third),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(webhookAddr, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(webhookAddr, "id", differentFrom(&id)),
					resource.TestCheckResourceAttr(webhookAddr, "event_types.#", "1"),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "1"),
					checkSigning(third, ""),
				),
			},
			{Config: webhookConfig(url, events[:1], 3, ""), ConfigVariables: webhookVars(third), PlanOnly: true},
		},
	})
}

// A configuration can let go of the secret, which sends nothing and leaves
// the platform signing with it, and take one back later, which is a rotation
// of the same subscription. The filter is left out, which subscribes to every
// event: the platform answers that as an empty list, and the attribute has to
// stay null rather than plan a change to it.
func TestAccWebhookSubscriptionLetsGoOfItsSecret(t *testing.T) {
	url := webhookURL("secret-later")
	first, second := webhookSecret("held"), webhookSecret("later")
	var id string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{
			{
				Config:          webhookConfig(url, nil, 1, ""),
				ConfigVariables: webhookVars(first),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(webhookAddr, "event_types"),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "1"),
					checkWebhookRow(func(row webhookRow) error {
						if len(row.EventTypes) != 0 {
							return fmt.Errorf("the subscription filters on %v, want every event", row.EventTypes)
						}
						return nil
					}),
					func(s *terraform.State) error {
						id = s.RootModule().Resources[webhookAddr].Primary.ID
						return nil
					},
				),
			},
			{
				// Letting go of the secret sends nothing: the platform keeps
				// signing with it.
				Config: webhookConfig(url, nil, 0, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(webhookAddr, "id", sameAs(&id)),
					resource.TestCheckNoResourceAttr(webhookAddr, "secret_wo_version"),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "1"),
					checkSigning(first, ""),
				),
			},
			{Config: webhookConfig(url, nil, 0, ""), PlanOnly: true},
			{
				Config:          webhookConfig(url, nil, 1, ""),
				ConfigVariables: webhookVars(second),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(webhookAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(webhookAddr, "id", sameAs(&id)),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "2"),
					checkSigning(second, first),
					checkNoSecretInState(first, second),
				),
			},
			{Config: webhookConfig(url, nil, 1, ""), ConfigVariables: webhookVars(second), PlanOnly: true},
		},
	})
}

// An import can't know the secret the platform signs with, so a configuration
// that names one plans a single in-place update: the import adopts the
// subscription and the same apply rotates it to the configured secret. The
// subscription and its id are kept, and the plan after it is empty.
func TestAccWebhookSubscriptionImportThenSecret(t *testing.T) {
	url := webhookURL("import-then-secret")
	secret := webhookSecret("adopted")
	adoptedAddr := webhookType + ".adopted"
	var id string
	adopted := strings.Replace(webhookConfig(url, nil, 1, ""), `"test"`, `"adopted"`, 1)
	adoption := adopted + fmt.Sprintf(`
variable "adopted_id" {
  type = string
}
removed {
  from = %[1]s.test
  lifecycle { destroy = false }
}
import {
  to = %[1]s.adopted
  identity = {
    id = var.adopted_id
  }
}
`, webhookType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{
			{
				// Made under a secret this configuration doesn't hold, as
				// another tool would have made it.
				Config: webhookConfigFixedSecret(url, nil, "made-elsewhere"),
				Check: func(s *terraform.State) error {
					id = s.RootModule().Resources[webhookAddr].Primary.ID
					return nil
				},
			},
			{
				Config: adoption,
				ConfigVariables: config.Variables{
					"adopted_id":     lazyString{&id},
					"webhook_secret": config.StringVariable(secret),
				},
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(adoptedAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(adoptedAddr, "id", sameAs(&id)),
					resource.TestCheckResourceAttr(adoptedAddr, "secret_version", "2"),
					func(*terraform.State) error {
						row, err := readWebhookRow(id)
						if err != nil {
							return err
						}
						if row.SigningSecret != secret {
							return fmt.Errorf("the adopted subscription doesn't sign with the configured secret")
						}
						return nil
					},
					checkNoSecretInState(secret),
				),
			},
			{Config: adopted, ConfigVariables: webhookVars(secret), PlanOnly: true},
		},
	})
}

// Every value the contract refuses is refused at plan time, naming the
// attribute, before anything is sent.
func TestAccWebhookSubscriptionRefusesAtPlan(t *testing.T) {
	url := webhookURL("refused")
	secret := webhookSecret("refused")
	for name, c := range map[string]struct {
		config string
		vars   config.Variables
		want   *regexp.Regexp
	}{
		"a scheme other than http or https": {
			webhookConfig("ftp://acc-hooks.example.test/x", nil, 0, ""), nil, regexp.MustCompile(`http`),
		},
		"a relative url": {
			webhookConfig("/hooks/x", nil, 0, ""), nil, regexp.MustCompile(`absolute`),
		},
		"credentials in the url": {
			webhookConfig("https://user:pass@acc-hooks.example.test/x", nil, 0, ""), nil, regexp.MustCompile(`password`),
		},
		"a new subscription with no secret": {
			webhookConfig(url, nil, 0, ""), nil, regexp.MustCompile(`Terraform 1\.11`),
		},
		"an empty filter": {
			webhookConfig(url, []string{}, 0, ""), nil, regexp.MustCompile(`every event`),
		},
		"a secret shorter than 32 characters": {
			webhookConfig(url, nil, 1, ""), webhookVars("too-short"), regexp.MustCompile(`32`),
		},
		"a secret with no version": {
			strings.Replace(webhookConfig(url, nil, 1, ""), "  secret_wo_version = 1\n", "", 1),
			webhookVars(secret), regexp.MustCompile(`secret_wo_version`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{{
					Config:          c.config,
					ConfigVariables: c.vars,
					PlanOnly:        true,
					ExpectError:     c.want,
				}},
			})
		})
	}
}

// The version can come from a variable. Terraform checks a configuration with
// every variable unknown before it plans, so refusing an unknown version there
// refused every variable, which the error itself said was fine.
func TestAccWebhookSubscriptionVersionFromVariable(t *testing.T) {
	url := webhookURL("version-variable")
	first, second := webhookSecret("var-first"), webhookSecret("var-second")
	cfg := strings.Replace(webhookConfig(url, nil, 1, ""), "secret_wo_version = 1", "secret_wo_version = var.secret_version", 1) +
		"\nvariable \"secret_version\" {\n  type = number\n}\n"
	vars := func(secret string, version int) config.Variables {
		v := webhookVars(secret)
		v["secret_version"] = config.IntegerVariable(version)
		return v
	}
	var id string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{
			{
				Config:          cfg,
				ConfigVariables: vars(first, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(webhookAddr, "secret_wo_version", "1"),
					checkSigning(first, ""),
					func(s *terraform.State) error {
						id = s.RootModule().Resources[webhookAddr].Primary.ID
						return nil
					},
				),
			},
			{
				Config:          cfg,
				ConfigVariables: vars(second, 2),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(webhookAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith(webhookAddr, "id", sameAs(&id)),
					resource.TestCheckResourceAttr(webhookAddr, "secret_wo_version", "2"),
					resource.TestCheckResourceAttr(webhookAddr, "secret_version", "2"),
					checkSigning(second, first),
					checkNoSecretInState(first, second),
				),
			},
			{Config: cfg, ConfigVariables: vars(second, 2), PlanOnly: true},
		},
	})
}

// A version only the apply works out is still refused at plan time, naming the
// attribute, because the plan couldn't say whether the secret rotates.
func TestAccWebhookSubscriptionVersionUnknownAtPlan(t *testing.T) {
	cfg := strings.Replace(webhookConfig(webhookURL("version-unknown"), nil, 1, ""),
		"secret_wo_version = 1", "secret_wo_version = length(terraform_data.version.id) > 0 ? 1 : 2", 1) +
		"\nresource \"terraform_data\" \"version\" {}\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:          cfg,
			ConfigVariables: webhookVars(webhookSecret("unknown")),
			PlanOnly:        true,
			ExpectError:     regexp.MustCompile(`secret_wo_version has to be known at plan time`),
		}},
	})
}

// Both read-only forms: one subscription by id, with the delivery health the
// resource leaves out, and the list, narrowed to one endpoint.
func TestAccWebhookSubscriptionDataSources(t *testing.T) {
	url := webhookURL("read-only")
	events := []string{"io.k0rdent.cluster.ready.v1"}
	config := webhookConfig(url, events, 1, "") + fmt.Sprintf(`
data %[1]q "one" {
  id = %[3]s.test.id
}

data %[2]q "endpoint" {
  url        = %[3]s.test.url
  depends_on = [%[3]s.test]
}

data %[2]q "all" {
  depends_on = [%[3]s.test]
}
`, webhookDataType, webhooksDataType, webhookType)
	one, byURL, all := "data."+webhookDataType+".one", "data."+webhooksDataType+".endpoint", "data."+webhooksDataType+".all"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{{
			Config:          config,
			ConfigVariables: webhookVars(webhookSecret("data")),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttrPair(one, "id", webhookAddr, "id"),
				resource.TestCheckResourceAttr(one, "url", url),
				resource.TestCheckResourceAttr(one, "event_types.#", "1"),
				resource.TestCheckResourceAttr(one, "secret_version", "1"),
				resource.TestCheckResourceAttr(one, "is_active", "true"),
				resource.TestCheckResourceAttr(one, "consecutive_failures", "0"),
				resource.TestCheckNoResourceAttr(one, "failing_since"),
				resource.TestCheckResourceAttr(byURL, "subscriptions.#", "1"),
				resource.TestCheckResourceAttrPair(byURL, "subscriptions.0.id", webhookAddr, "id"),
				resource.TestCheckResourceAttr(byURL, "subscriptions.0.url", url),
				resource.TestMatchResourceAttr(all, "subscriptions.#", regexp.MustCompile(`^[1-9][0-9]*$`)),
			),
		}},
	})
}

// webhookDeliveryHistory, in a subscription's URL, makes the fake give it a
// delivery history of five, newest first: queued, failed with a retry due,
// failed and dead-lettered, then delivered twice. The fake sends no events,
// so nothing else has one.
const webhookDeliveryHistory = "fake-delivery-history"

// A subscription's delivery history: the newest `limit`, read across the
// fake's pages of two, and the failed ones among them, under a secret that
// reaches no delivery.
func TestAccWebhookDeliveriesDataSource(t *testing.T) {
	secret := webhookSecret("deliveries")
	config := webhookConfig(webhookURL("deliveries-"+webhookDeliveryHistory), nil, 1, "") + fmt.Sprintf(`
data %[1]q "newest" {
  subscription_id = %[2]s.test.id
  limit           = 3
}

data %[1]q "failed" {
  subscription_id = %[2]s.test.id
  status          = "failed"
}
`, webhookDeliveriesDataType, webhookType)
	newest, failed := "data."+webhookDeliveriesDataType+".newest", "data."+webhookDeliveriesDataType+".failed"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{
			{
				Config:          config,
				ConfigVariables: webhookVars(secret),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(newest, "deliveries.#", "3"),
					resource.TestCheckResourceAttr(newest, "deliveries.0.status", "queued"),
					resource.TestCheckResourceAttr(newest, "deliveries.0.attempts", "0"),
					resource.TestCheckNoResourceAttr(newest, "deliveries.0.response_code"),
					resource.TestCheckNoResourceAttr(newest, "deliveries.0.settled_at"),
					resource.TestCheckResourceAttr(newest, "deliveries.1.status", "failed"),
					resource.TestCheckResourceAttr(newest, "deliveries.1.response_code", "503"),
					resource.TestCheckResourceAttr(newest, "deliveries.1.retry_scheduled", "true"),
					resource.TestCheckResourceAttrSet(newest, "deliveries.1.next_attempt_at"),
					resource.TestCheckResourceAttr(newest, "deliveries.2.status", "failed"),
					resource.TestCheckResourceAttr(newest, "deliveries.2.retry_scheduled", "false"),
					resource.TestCheckResourceAttr(newest, "deliveries.2.last_error", "dial tcp: connection refused"),
					resource.TestCheckResourceAttrSet(newest, "deliveries.2.settled_at"),
					// Of the default 50, which the fake's five fall inside.
					resource.TestCheckResourceAttr(failed, "deliveries.#", "2"),
					resource.TestCheckResourceAttrPair(failed, "deliveries.0.id", newest, "deliveries.1.id"),
					resource.TestCheckResourceAttrPair(failed, "deliveries.1.id", newest, "deliveries.2.id"),
					checkNoSecretInState(secret),
				),
			},
			{
				// The id has to be one the organization has: the operation
				// would answer an empty page for any other.
				Config: config + fmt.Sprintf(`
data %q "missing" {
  subscription_id = "0f8fad5b-d9cb-469f-a165-70867728950e"
}
`, webhookDeliveriesDataType),
				ConfigVariables: webhookVars(secret),
				ExpectError:     regexp.MustCompile(`No such webhook subscription`),
			},
		},
	})
}

// A subscription somebody deleted outside Terraform reads as gone, and the
// next plan makes it again rather than failing.
func TestAccWebhookSubscriptionDeletedOutsideTerraform(t *testing.T) {
	config := webhookConfig(webhookURL("deleted-outside"), nil, 1, "")
	vars := webhookVars(webhookSecret("outside"))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkWebhookGone,
		Steps: []resource.TestStep{
			{Config: config, ConfigVariables: vars},
			{
				PreConfig: func() {
					if err := deleteWebhookOutOfBand(t); err != nil {
						t.Fatal(err)
					}
				},
				Config:             config,
				ConfigVariables:    vars,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// deleteWebhookOutOfBand deletes every subscription to the deleted-outside
// endpoint through the API, as another tool would.
func deleteWebhookOutOfBand(t *testing.T) error {
	t.Helper()
	token, err := mintFakeToken()
	if err != nil {
		return err
	}
	status, body, err := getFake("/v1/regions/global/notifications/webhooks", token)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("listing subscriptions answered %d: %v", status, err)
	}
	var list struct {
		Subscriptions []webhookRow `json:"subscriptions"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return err
	}
	for _, s := range list.Subscriptions {
		if s.URL != webhookURL("deleted-outside") {
			continue
		}
		req, err := http.NewRequest(http.MethodDelete, endpoint+"/v1/regions/global/notifications/webhooks/"+s.ID, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := harnessHTTP.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("deleting %s answered %s", s.ID, resp.Status)
		}
	}
	return nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sameAs and differentFrom compare an attribute with a value an earlier step
// recorded.
func sameAs(want *string) resource.CheckResourceAttrWithFunc {
	return func(got string) error {
		if got != *want {
			return fmt.Errorf("%q changed to %q, so this was a new subscription", *want, got)
		}
		return nil
	}
}

func differentFrom(old *string) resource.CheckResourceAttrWithFunc {
	return func(got string) error {
		if got == *old {
			return fmt.Errorf("still %q, so nothing was replaced", got)
		}
		return nil
	}
}

// lazyString is a configuration variable whose value is read when the step
// runs rather than when the steps are built, for an id an earlier step learned.
type lazyString struct{ value *string }

func (v lazyString) MarshalJSON() ([]byte, error) { return json.Marshal(*v.value) }
