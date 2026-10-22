package provider

import (
	"strings"
	"testing"
)

// The sweepers get their client here, so it has to apply the same rules a
// provider block with nothing in it would: the environment's targeting, and
// the same refusals.
func TestMetaFromEnvironmentReadsTheEnvironment(t *testing.T) {
	t.Setenv(EnvVar(envEndpoint), "https://api.dev02.example.com")
	t.Setenv(EnvVar(envRegion), "us-texas-1")
	t.Setenv(EnvVar(envProject), "demo")
	t.Setenv(EnvVar(envClientID), "sa-terraform@vendor.iam")
	t.Setenv(EnvVar(envClientSecret), "k0r_secret")
	t.Setenv(EnvVar(envAPIKey), "")

	meta, err := MetaFromEnvironment(UserAgent("sweep", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Endpoint != "https://api.dev02.example.com" || meta.Region != "us-texas-1" || meta.Project != "demo" {
		t.Errorf("got endpoint %q, region %q, project %q", meta.Endpoint, meta.Region, meta.Project)
	}
	if meta.Client == nil {
		t.Error("no client")
	}
}

func TestMetaFromEnvironmentRefusesWhatThePlanWould(t *testing.T) {
	t.Setenv(EnvVar(envEndpoint), "http://api.dev02.example.com")
	t.Setenv(EnvVar(envClientID), "sa-terraform@vendor.iam")
	t.Setenv(EnvVar(envClientSecret), "k0r_secret")
	t.Setenv(EnvVar(envAPIKey), "")

	_, err := MetaFromEnvironment(UserAgent("sweep", "", ""))
	if err == nil {
		t.Fatal("a plain-http remote endpoint was accepted")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("the refusal doesn't say what was wrong: %v", err)
	}
}
