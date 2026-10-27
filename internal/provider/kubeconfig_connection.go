package provider

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.yaml.in/yaml/v3"
)

// kubeconfigConnection is what the kubernetes and helm providers take in their
// own configuration, under their own names. An empty field is one the
// kubeconfig doesn't carry, or one this provider couldn't read out of it.
//
// The certificates are PEM, decoded from the kubeconfig's base64, because PEM
// is what those providers' `cluster_ca_certificate`, `client_certificate` and
// `client_key` take. A configuration hands them over as they are.
type kubeconfigConnection struct {
	Host                 string
	ClusterCACertificate string
	Token                string
	ClientCertificate    string
	ClientKey            string

	// No attribute carries these. The relay connects with them.
	TLSServerName string
	ProxyURL      string
}

type kubeconfigNames struct{ context, cluster, user, namespace string }

// currentKubeconfigNames picks the context as parseKubeconfigConnection does.
func currentKubeconfigNames(raw string) kubeconfigNames {
	var file kubeconfigFile
	if yaml.Unmarshal([]byte(raw), &file) != nil {
		return kubeconfigNames{}
	}
	name := file.CurrentContext
	if name == "" && len(file.Contexts) == 1 {
		name = file.Contexts[0].Name
	}
	for _, c := range file.Contexts {
		if c.Name == name {
			return kubeconfigNames{context: name, cluster: c.Context.Cluster, user: c.Context.User, namespace: c.Context.Namespace}
		}
	}
	return kubeconfigNames{}
}

// relayKubeconfig is `raw` with `relay` on. It keeps the platform's names, so
// a check on `current-context` reads the same, and none of its credentials.
func relayKubeconfig(names kubeconfigNames, host, caPEM, secret string) (string, error) {
	type namedCluster struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	}
	type namedContext struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace,omitempty"`
		} `yaml:"context"`
	}
	type namedUser struct {
		Name string `yaml:"name"`
		User struct {
			Token string `yaml:"token"`
		} `yaml:"user"`
	}
	var file struct {
		APIVersion     string         `yaml:"apiVersion"`
		Kind           string         `yaml:"kind"`
		Clusters       []namedCluster `yaml:"clusters"`
		Contexts       []namedContext `yaml:"contexts"`
		Users          []namedUser    `yaml:"users"`
		CurrentContext string         `yaml:"current-context"`
	}
	file.APIVersion, file.Kind, file.CurrentContext = "v1", "Config", names.context
	var c namedCluster
	c.Name, c.Cluster.Server = names.cluster, host
	c.Cluster.CAData = base64.StdEncoding.EncodeToString([]byte(caPEM))
	var x namedContext
	x.Name, x.Context.Cluster, x.Context.User, x.Context.Namespace = names.context, names.cluster, names.user, names.namespace
	var u namedUser
	u.Name, u.User.Token = names.user, secret
	file.Clusters, file.Contexts, file.Users = []namedCluster{c}, []namedContext{x}, []namedUser{u}
	out, err := yaml.Marshal(file)
	return string(out), err
}

// The fields of a kubeconfig this reads. Everything else is left in `raw`.
type kubeconfigFile struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data"`
			CAFile string `yaml:"certificate-authority"`
			// Not handed over. parseKubeconfigConnection warns when one is set.
			InsecureSkipTLSVerify bool   `yaml:"insecure-skip-tls-verify"`
			TLSServerName         string `yaml:"tls-server-name"`
			ProxyURL              string `yaml:"proxy-url"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string         `yaml:"name"`
		User kubeconfigUser `yaml:"user"`
	} `yaml:"users"`
}

type kubeconfigUser struct {
	Token          string `yaml:"token"`
	TokenFile      string `yaml:"tokenFile"`
	ClientCertData string `yaml:"client-certificate-data"`
	ClientCertFile string `yaml:"client-certificate"`
	ClientKeyData  string `yaml:"client-key-data"`
	ClientKeyFile  string `yaml:"client-key"`
	Username       string `yaml:"username"`
	// Only whether there is one. Its command line is never read, so it can't
	// end up in a warning.
	Exec         *struct{} `yaml:"exec"`
	AuthProvider *struct {
		Name string `yaml:"name"`
	} `yaml:"auth-provider"`
}

// parseKubeconfigConnection reads the connection a kubeconfig's current context
// names. With no `current-context`, a file with exactly one context uses that
// one. That's a deliberate leniency: kubectl refuses such a file, but with one
// context there's only one cluster it could mean.
//
// It never fails the open. `raw` is the contract this resource had first, and a
// configuration that reads it has to keep working whatever the parsed half
// makes of the body. So a problem comes back as a warning.
//
// It fails closed. Whenever there's no context to choose or no credential to
// hand over, every field stays empty, `host` included. The kubernetes and helm
// providers fill an argument left null from KUBE_TOKEN, KUBE_CONFIG_PATH and
// the like, so a `host` handed over without a credential would send a local
// credential to the platform's cluster. With nothing set, no local credential
// reaches the platform's cluster, but a provider wired from these still isn't
// stopped: it fills `host` from KUBE_HOST, KUBE_CONFIG_PATH(S) and KUBE_CTX
// too, and reaches whatever cluster those name. A configuration stops that
// with a postcondition on the ephemeral block, which the example shows.
//
// No warning quotes a value from the user entry, an exec plugin's command line
// included. A warning is printed to the console and the trace log, where
// `Sensitive` doesn't reach.
func parseKubeconfigConnection(raw string) (kubeconfigConnection, []string) {
	return parseKubeconfigConnectionFor(raw, false)
}

// parseKubeconfigConnectionFor warns as the relay will connect when relay is
// set and there's a token.
func parseKubeconfigConnectionFor(raw string, relay bool) (kubeconfigConnection, []string) {
	var file kubeconfigFile
	// The parser's own error isn't quoted: it can carry a fragment of the
	// value it choked on, and here that can be the token.
	if err := yaml.Unmarshal([]byte(raw), &file); err != nil {
		return unusableKubeconfig("The kubeconfig is not YAML in a kubeconfig's shape.")
	}

	contextName := file.CurrentContext
	if contextName == "" {
		switch len(file.Contexts) {
		case 0:
			return unusableKubeconfig("The kubeconfig has no contexts.")
		case 1:
			contextName = file.Contexts[0].Name
		default:
			return unusableKubeconfig(fmt.Sprintf("The kubeconfig names no current-context and has %d "+
				"contexts, so which cluster to connect to is not this provider's choice.", len(file.Contexts)))
		}
	}

	ctxIndex := -1
	for i, c := range file.Contexts {
		if c.Name == contextName {
			ctxIndex = i
			break
		}
	}
	if ctxIndex < 0 {
		return unusableKubeconfig(fmt.Sprintf("The kubeconfig's current-context %q is not among its contexts.", contextName))
	}
	ctx := file.Contexts[ctxIndex].Context

	clusterIndex, userIndex := -1, -1
	for i, c := range file.Clusters {
		if c.Name == ctx.Cluster {
			clusterIndex = i
			break
		}
	}
	for i, u := range file.Users {
		if u.Name == ctx.User {
			userIndex = i
			break
		}
	}
	switch {
	case clusterIndex < 0:
		return unusableKubeconfig(fmt.Sprintf("The kubeconfig's context %q names cluster %q, which is not in the file.",
			contextName, ctx.Cluster))
	case userIndex < 0:
		return unusableKubeconfig(fmt.Sprintf("The kubeconfig's context %q names user %q, which is not in the file.",
			contextName, ctx.User))
	}
	cluster := file.Clusters[clusterIndex].Cluster
	if cluster.Server == "" {
		return unusableKubeconfig(fmt.Sprintf("The kubeconfig's cluster %q has no server.", ctx.Cluster))
	}

	// The credential comes first, because without one nothing is handed over.
	user := file.Users[userIndex].User
	conn := kubeconfigConnection{Host: cluster.Server, Token: user.Token}
	if user.ClientCertData != "" && user.ClientKeyData != "" {
		cert, certErr := base64.StdEncoding.DecodeString(user.ClientCertData)
		key, keyErr := base64.StdEncoding.DecodeString(user.ClientKeyData)
		switch {
		case certErr == nil && keyErr == nil:
			conn.ClientCertificate, conn.ClientKey = string(cert), string(key)
		case conn.Token == "":
			return unusableKubeconfig(fmt.Sprintf("The kubeconfig's user %q has a client certificate or key "+
				"that is not base64.", ctx.User))
		}
	}
	if conn.Token == "" && conn.ClientCertificate == "" {
		return unusableKubeconfig(describeUnusableUser(ctx.User, user))
	}
	conn.TLSServerName, conn.ProxyURL = cluster.TLSServerName, cluster.ProxyURL
	relayed := relay && conn.Token != ""

	// A CA the provider can't hand over leaves `cluster_ca_certificate` null,
	// and then the kubernetes provider checks the server against the system
	// roots. That fails closed too, on the TLS handshake, so the rest stays.
	var warnings []string
	noCA := "`cluster_ca_certificate` is null"
	if relayed {
		noCA = "the relay checks the cluster's certificate against the system roots"
	}
	switch {
	case cluster.CAData != "":
		ca, err := base64.StdEncoding.DecodeString(cluster.CAData)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("In the kubeconfig's cluster %q, "+
				"certificate-authority-data is not base64, so %s.", ctx.Cluster, noCA))
		} else {
			conn.ClusterCACertificate = string(ca)
		}
	case cluster.CAFile != "":
		// A path names a file on whatever machine wrote the kubeconfig.
		warnings = append(warnings, fmt.Sprintf("The kubeconfig's cluster %q names a CA file (%q) "+
			"rather than carrying the certificate, so %s.", ctx.Cluster, cluster.CAFile, noCA))
	}
	if relayed {
		// The relay uses tls-server-name and proxy-url, and always verifies.
		if cluster.InsecureSkipTLSVerify {
			warnings = append(warnings, fmt.Sprintf("The kubeconfig's cluster %q sets insecure-skip-tls-verify, "+
				"which the relay doesn't honor: it checks the cluster's certificate against the kubeconfig's "+
				"CA, or the system roots without one.", ctx.Cluster))
		}
		return conn, warnings
	}
	for _, d := range []struct {
		set               bool
		setting, argument string
	}{
		{cluster.InsecureSkipTLSVerify, "insecure-skip-tls-verify", "insecure"},
		{cluster.TLSServerName != "", "tls-server-name", "tls_server_name"},
		{cluster.ProxyURL != "", "proxy-url", "proxy_url"},
	} {
		// Named, never quoted: a proxy URL can carry a password.
		if d.set {
			warnings = append(warnings, fmt.Sprintf("The kubeconfig's cluster %q sets %s, which no parsed "+
				"attribute carries. A kubernetes or helm provider configured from them connects without it "+
				"unless its `%s` argument is set, from `raw` through `yamldecode` or by hand.",
				ctx.Cluster, d.setting, d.argument))
		}
	}
	return conn, warnings
}

// describeUnusableUser says what a user entry carries when it carries nothing
// this provider hands over, because that decides what to do instead. It names
// each mechanism and quotes none of its values: an exec plugin's command line
// can hold a secret of its own.
func describeUnusableUser(name string, user kubeconfigUser) string {
	var carries []string
	if user.Exec != nil {
		carries = append(carries, "an exec plugin")
	}
	if user.AuthProvider != nil {
		carries = append(carries, fmt.Sprintf("the %q auth provider", user.AuthProvider.Name))
	}
	if user.TokenFile != "" {
		carries = append(carries, "a token file")
	}
	if user.ClientCertFile != "" || user.ClientKeyFile != "" {
		carries = append(carries, "a client certificate file")
	}
	if user.ClientCertData != "" || user.ClientKeyData != "" {
		carries = append(carries, "half of a client certificate and key")
	}
	if user.Username != "" {
		carries = append(carries, "a username and password")
	}
	if len(carries) == 0 {
		return fmt.Sprintf("The kubeconfig's user %q carries no credential.", name)
	}
	return fmt.Sprintf("The kubeconfig's user %q authenticates with %s, which this provider does not "+
		"run or read.", name, strings.Join(carries, " and "))
}

// unusableKubeconfig is the fail-closed answer: nothing handed over, and a
// warning that says why and what to use instead.
func unusableKubeconfig(why string) (kubeconfigConnection, []string) {
	return kubeconfigConnection{}, []string{why + " Every parsed attribute, `host` included, is null. A " +
		"kubernetes or helm provider configured from them falls back to `KUBE_HOST`, `KUBE_TOKEN`, " +
		"`KUBE_CONFIG_PATH` and `KUBE_CTX`, and reaches whatever cluster those name, unless a " +
		"postcondition on this block stops the run. `raw` holds the kubeconfig as the platform sent " +
		"it. A user that authenticates with an exec plugin can be wired from it, through `yamldecode`, " +
		"into the kubernetes and helm providers' own `exec` block."}
}

// emptyAsNull is a parsed field as Terraform should see it. A field the
// kubeconfig doesn't carry is null rather than "", so a provider configured
// from it falls back to its own default instead of taking an empty value.
func emptyAsNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
