package sspanel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XrayR-project/XrayR/api"
)

func TestMemoryUserResponseValidation(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"users", `{"ret":1,"data":[{"id":1,"email":"user@example.invalid","uuid":"uuid","node_connector":3,"node_speedlimit":8}]}`, 200, true},
		{"empty", `{"ret":1,"data":[]}`, 200, true},
		{"null", `{"ret":1,"data":null}`, 200, true},
		{"missing_data", `{"ret":1}`, 200, false},
		{"missing_ret", `{"data":[]}`, 200, false},
		{"ret_failure", `{"ret":0,"data":[]}`, 200, false},
		{"ret_failure_message", `{"ret":0,"data":"denied"}`, 200, false},
		{"wrong_data_type", `{"ret":1,"data":{}}`, 200, false},
		{"wrong_user_type", `{"ret":1,"data":[{"id":"invalid"}]}`, 200, false},
		{"malformed", `{"ret":1,"data":[`, 200, false},
		{"http_failure", `{"ret":1,"data":[]}`, 500, false},
		{"no_content", "", 204, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/mod_mu/users" || r.URL.Query().Get("node_id") != "251" {
					t.Error("changed endpoint/node ID")
				}
				w.Header().Set("ETag", "validated")
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			c := New(&api.Config{APIHost: server.URL, NodeID: 251})
			defer c.Close()
			c.client.SetRetryCount(0)
			users, err := c.GetUserList()
			if test.valid {
				if err != nil || users == nil {
					t.Fatalf("valid response failed: %v", err)
				}
				if c.eTags["users"] != "validated" {
					t.Fatal("valid ETag was not remembered")
				}
				if test.name == "users" && (len(*users) != 1 || (*users)[0].UID != 1 || (*users)[0].DeviceLimit != 3 || (*users)[0].SpeedLimit != 1000000) {
					t.Fatal("user parameters changed")
				}
			} else if err == nil || users != nil || c.eTags["users"] != "" {
				t.Fatalf("invalid response accepted or cached: users=%v err=%v etag=%q", users, err, c.eTags["users"])
			}
		})
	}
}

func TestMemoryUserETagAndShadowsocksParsing(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 2 {
			if r.Header.Get("If-None-Match") != "users-v1" {
				t.Error("missing conditional request")
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if requests == 3 && r.Header.Get("If-None-Match") != "" {
			t.Error("SS node parsing must fetch the port unconditionally")
		}
		w.Header().Set("ETag", "users-v1")
		fmt.Fprint(w, `{"ret":1,"data":[{"id":1,"port":12345,"node_connector":3,"alive_ip":3}]}`)
	}))
	defer server.Close()
	c := New(&api.Config{APIHost: server.URL, NodeID: 251})
	defer c.Close()
	c.LastReportOnline[1] = 1
	users, err := c.GetUserList()
	if err != nil || len(*users) != 1 || (*users)[0].DeviceLimit != 1 || len(c.LastReportOnline) != 0 {
		t.Fatal("device filtering or online reset changed", err)
	}
	c.LastReportOnline[1] = 1
	if users, err := c.GetUserList(); users != nil || err == nil || err.Error() != api.UserNotModified || c.LastReportOnline[1] != 1 {
		t.Fatal("304 behavior changed", err)
	}
	node, err := c.ParseSSNodeResponse(&NodeInfoResponse{})
	if err != nil || node.Port != 12345 {
		t.Fatal("SS port parsing changed", err)
	}
}

func TestMemoryUserNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	c := New(&api.Config{APIHost: url, NodeID: 251})
	defer c.Close()
	c.client.SetRetryCount(0)
	if users, err := c.GetUserList(); users != nil || err == nil || !strings.Contains(err.Error(), "request ") {
		t.Fatal("network failure must return an error, not panic or remove users", err)
	}
}

func BenchmarkMemoryParseUserList(b *testing.B) {
	users := make([]UserResponse, 6400)
	for i := range users {
		users[i] = UserResponse{ID: i + 1, DeviceLimit: 3, SpeedLimit: 8}
	}
	c := &APIClient{}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.ParseUserListResponse(&users); err != nil {
			b.Fatal(err)
		}
	}
}

func TestMemoryFilteredUserCapacity(t *testing.T) {
	response := make([]UserResponse, 6400)
	for i := range response {
		response[i] = UserResponse{ID: i + 1, DeviceLimit: 1, AliveIP: 1}
	}
	c := &APIClient{}
	users, err := c.ParseUserListResponse(&response)
	if err != nil || *users != nil {
		t.Fatal("fully filtered users should not retain their preallocated array", err)
	}
	response[0].AliveIP = 0
	users, err = c.ParseUserListResponse(&response)
	if err != nil || len(*users) != 1 || cap(*users) > 2 {
		t.Fatal("sparse accepted users retained an oversized array", err)
	}
}
