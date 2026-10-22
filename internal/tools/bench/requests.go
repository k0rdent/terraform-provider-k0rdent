package main

import (
	"bufio"
	"io"
	"path"
	"strings"
)

// requestCounts is what the fake was asked during one Terraform command.
//
// The split matters more than the total. A refresh that reads each object once
// makes one item read per resource whatever the size. One that finds each
// object by listing them all makes one list per resource, each list as long as
// the estate, and that's the quadratic shape this benchmark is here to catch.
// So lists are counted on their own, by the collection they read.
type requestCounts struct {
	Total     int            `json:"total"`
	Mints     int            `json:"mints"`
	ItemReads int            `json:"item_reads"`
	ListReads int            `json:"list_reads"`
	Writes    int            `json:"writes"`
	Lists     map[string]int `json:"lists,omitempty"`
}

// collections are the last path segments that name a list rather than one
// object. Every route in internal/tools/fakeapi/main.go that answers a GET on
// a collection ends in one of these.
var collections = map[string]bool{
	"regions": true, "projects": true, "organizations": true, "roles": true,
	"service-accounts": true, "credentials": true, "cluster-types": true,
	"versions": true, "address-pools": true, "clusters": true, "events": true,
	"kubeconfigs": true, "instance-groups": true, "instances": true,
	"networks": true, "subnets": true, "vpcs": true, "peerings": true,
	"security-groups": true, "security-groups-effective": true,
	"attachments": true, "share-types": true, "file-stores": true, "webhooks": true,
}

// countRequests reads the fake's request log. Each request is one line, as
// logging in internal/tools/fakeapi/main.go writes it:
//
//	2026/10/11 09:00:00 GET /v1/regions/us-texas-1/... ua="..." auth=present
//
// Anything else on the log (a startup line, a refusal the guard wrote) isn't
// a request and isn't counted. Neither is a harness route.
func countRequests(r io.Reader) (requestCounts, error) {
	c := requestCounts{Lists: map[string]int{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.HasPrefix(f[3], "/v1/") || !isMethod(f[2]) {
			continue
		}
		method, p := f[2], f[3]
		c.Total++
		switch {
		case method == "POST" && p == "/v1/regions/global/auth/token":
			c.Mints++
		case method == "GET" && collections[path.Base(p)]:
			c.ListReads++
			c.Lists[path.Base(p)]++
		case method == "GET":
			c.ItemReads++
		default:
			c.Writes++
		}
	}
	return c, sc.Err()
}

func isMethod(s string) bool {
	switch s {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		return true
	}
	return false
}
