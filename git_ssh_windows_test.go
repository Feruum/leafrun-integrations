package integrations

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"testing"
)

func restrictTestPrivateKey(t *testing.T, name string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	cmd := exec.Command("icacls", name, "/inheritance:r", "/grant:r", "*"+sid+":(R)")
	hideFixtureProcess(cmd)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("owned test key ACL: %v %s", err, data)
	}
}
