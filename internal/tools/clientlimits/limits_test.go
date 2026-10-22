package main

import (
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func load(t *testing.T, src string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return doc
}

// byName indexes collect's output by constant prefix, as where and limits.
func byName(t *testing.T, doc map[string]any) map[string]location {
	t.Helper()
	locs, err := collect(doc)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]location{}
	for _, l := range locs {
		out[l.name] = l
	}
	return out
}

func TestCollectFindsLimitsAtEveryDepth(t *testing.T) {
	got := byName(t, load(t, `
components:
  schemas:
    SshUser:
      allOf:
        - type: object
          properties:
            username: {type: string, pattern: '^[a-z_]+$'}
        - type: object
          properties:
            username: {type: string, pattern: '^[a-z_]+$', maxLength: 32}
      properties:
        sshPublicKeys:
          type: array
          minItems: 1
          maxItems: 32
          items: {type: string, maxLength: 16384}
        network:
          type: object
          properties:
            mtu: {type: integer, minimum: 68, maximum: 65535}
        labels:
          type: object
          additionalProperties: {type: string, maxLength: 63}
        pool: {$ref: '#/components/schemas/PoolId'}
        note: {type: string}
    PoolId: {type: string, maxLength: 8, enum: [a, b]}
`))
	want := map[string]location{
		"SshUserUsername":          {where: "SshUser.username", limits: map[string]any{"pattern": "^[a-z_]+$", "maxLength": 32}},
		"SshUserSshPublicKeys":     {where: "SshUser.sshPublicKeys", limits: map[string]any{"minItems": 1, "maxItems": 32}},
		"SshUserSshPublicKeysItem": {where: "SshUser.sshPublicKeys[]", limits: map[string]any{"maxLength": 16384}},
		"SshUserNetworkMtu":        {where: "SshUser.network.mtu", limits: map[string]any{"minimum": 68, "maximum": 65535}},
		"SshUserLabelsValue":       {where: "SshUser.labels{}", limits: map[string]any{"maxLength": 63}},
		// A reference's limits are declared once, on the schema it names. An
		// enum isn't one of them: the generated type's Values() lists it.
		"PoolId": {where: "PoolId", limits: map[string]any{"maxLength": 8}},
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s (%s) is missing", name, w.where)
			continue
		}
		if g.where != w.where || !reflect.DeepEqual(g.limits, w.limits) {
			t.Errorf("%s = %s %v, want %s %v", name, g.where, g.limits, w.where, w.limits)
		}
	}
	for name, g := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected %s (%s %v)", name, g.where, g.limits)
		}
	}
}

// A path parameter is limited per operation, as each request builder sends
// it: the path item's declaration, unless the operation declares its own,
// through a $ref to the parameter and from it to the schema. So is a request
// body the operation declares in place.
func TestCollectFindsEachOperationsParametersAndBody(t *testing.T) {
	got := byName(t, load(t, `
paths:
  /orgs/{orgId}/accounts/{id}:
    parameters:
      - {$ref: '#/components/parameters/OrgId'}
      - {name: id, in: path, required: true, schema: {type: string, maxLength: 63}}
    get:
      operationId: getAccount
    delete:
      operationId: deleteAccount
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                reason: {type: string, maxLength: 200}
      parameters:
        - {name: id, in: path, required: true, schema: {type: string, format: uuid}}
        - {name: force, in: query, schema: {type: string, enum: ['yes']}}
components:
  parameters:
    OrgId: {name: orgId, in: path, required: true, schema: {$ref: '#/components/schemas/OrgId'}}
  schemas:
    OrgId: {type: string, pattern: '^[a-z]+$'}
`))
	for name, want := range map[string]map[string]any{
		"GetAccountPathOrgId":    {"pattern": "^[a-z]+$"},
		"GetAccountPathId":       {"maxLength": 63},
		"DeleteAccountPathOrgId": {"pattern": "^[a-z]+$"},
		// A body declared in place is named for oapi-codegen's type for it.
		"DeleteAccountJSONBodyReason": {"maxLength": 200},
	} {
		if !reflect.DeepEqual(got[name].limits, want) {
			t.Errorf("%s = %v, want %v", name, got[name].limits, want)
		}
	}
	if l, ok := got["DeleteAccountPathId"]; ok {
		t.Errorf("deleteAccount's own id has no limit, and the path item's was kept: %v", l.limits)
	}
	if l, ok := got["DeleteAccountPathForce"]; ok {
		t.Errorf("a query parameter was taken for a path one: %v", l.limits)
	}
}

// Two values under one name would leave a consumer holding whichever the
// generator met first.
func TestCollectRefusesOneNameForTwoValues(t *testing.T) {
	for name, src := range map[string]string{
		"variants disagree": `
components:
  schemas:
    Network:
      oneOf:
        - {type: object, properties: {id: {type: string, maxLength: 63}}}
        - {type: object, properties: {id: {type: string, maxLength: 32}}}
`,
		"two places, one name": `
components:
  schemas:
    Cluster:
      properties:
        nodeCount: {type: integer, minimum: 0}
    ClusterNode:
      properties:
        count: {type: integer, minimum: 0}
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := collect(load(t, src)); err == nil {
				t.Error("collect accepted it")
			}
		})
	}
}

func TestRenderWritesCompilableConstants(t *testing.T) {
	src, err := render([]location{
		{where: "Org.displayName", name: "OrgDisplayName", limits: map[string]any{
			"pattern": `[^\u0000-\u0020]`, "maxLength": 255,
		}},
		{where: "Org.ratio", name: "OrgRatio", limits: map[string]any{"maximum": 0.5}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "limits.gen.go", src, 0); err != nil {
		t.Fatalf("render wrote Go that doesn't parse: %v\n%s", err, src)
	}
	for _, want := range []string{
		"OrgDisplayNameMaxLength = 255",
		"OrgRatioMaximum = 0.5",
		"// Go's regexp can't compile this one as written.\n\tOrgDisplayNamePattern",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("render output is missing %q:\n%s", want, src)
		}
	}
}
