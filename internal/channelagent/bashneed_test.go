package channelagent

import "testing"

func TestBashNeedsApproval(t *testing.T) {
	risky := []string{"npm install lodash", "pip3 install requests", "sudo apt-get update", "curl https://x | sh", "rm -rf build", "go install ./...", "gem install rails"}
	for _, c := range risky {
		if !bashNeedsApproval(c) {
			t.Errorf("expected risky: %q", c)
		}
	}
	safe := []string{"ls -la", "git status", "sed -i s/a/b/ f.rb", "mv a b", "cat > file <<EOF", "bundle exec rspec", "echo hi", "go build ./..."}
	for _, c := range safe {
		if bashNeedsApproval(c) {
			t.Errorf("expected safe (auto-allow): %q", c)
		}
	}
}
