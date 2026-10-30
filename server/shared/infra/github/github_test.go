package github

import "testing"

// Without a configured client id, GitHub CLI's public value is used; when configured, the
// configured one is used.
func TestClientIDDefault(t *testing.T) {
	c, err := New(Config{})
	if err != nil || c.ClientID() != DefaultClientID || DefaultClientID != "178c6fc778ccc68e1d6a" {
		t.Fatalf("留空应用默认 client id: %v %v", c, err)
	}
	c, err = New(Config{ClientID: " mine "})
	if err != nil || c.ClientID() != "mine" {
		t.Fatalf("配了就该用配的: %v %v", c, err)
	}
	if got := c.Scopes(); len(got) != 2 || got[0] != "read:user" || got[1] != "user:email" {
		t.Errorf("默认 scope 不对: %v", got)
	}
	if _, err := New(Config{Proxy: "127.0.0.1:8080"}); err == nil {
		t.Error("代理不带 scheme 应报错")
	}
}
