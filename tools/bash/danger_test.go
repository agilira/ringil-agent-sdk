// danger_test.go: which commands need the user's yes
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package bash

import "testing"

func TestDangerous(t *testing.T) {
	for cmd, want := range map[string]bool{
		"ls -la":                                  false,
		"go test ./... && git status":             false,
		"git diff HEAD~1 | head":                  false,
		"git commit -m 'rm -rf /'":                false,
		"git add -A && git commit -m x":           false,
		"git checkout -b feature":                 false,
		"git restore --staged a.go":               false,
		"git branch -d merged":                    false,
		"rm a.txt":                                false,
		"rm -f a.txt":                             false,
		"echo $(date)":                            false,
		"for f in *.go; do gofmt -l \"$f\"; done": false,
		"find . -name '*.go'":                     false,
		"timeout 30 go test ./...":                false,

		"rm -rf build":                 true,
		"rm -r -f build":               true,
		"rm --recursive build":         true,
		"/bin/rm -fr build":            true,
		"\\rm -rf build":               true,
		"r\\m -rf build":               true,
		"echo ok; rm -rf build":        true,
		"true && (cd x && rm -Rf y)":   true,
		"echo $(rm -rf x)":             true,
		"f() { rm -rf x; }; f":         true,
		"sudo rm -rf x":                true,
		"env FOO=1 rm -rf x":           true,
		"timeout 5s rm -rf x":          true,
		"xargs rm -rf < list":          true,
		"find . -name '*.o' -delete":   true,
		"find . -exec rm -rf {} +":     true,
		"bash -c 'rm -rf x'":           true,
		"sh -lc \"git push\"":          true,
		"bash -c \"$CMD\"":             true,
		"bash script.sh":               true,
		"eval \"$X\"":                  true,
		"$CMD -rf x":                   true,
		"r{m,} -rf x":                  true,
		"git push":                     true,
		"git -C repo push --force":     true,
		"git -c core.x=y push":         true,
		"git reset --hard HEAD~1":      true,
		"git clean -fdx":               true,
		"git checkout -- a.go":         true,
		"git checkout .":               true,
		"git restore a.go":             true,
		"git switch -f main":           true,
		"git branch -D feature":        true,
		"git stash drop":               true,
		"git rebase -i main":           true,
		"git commit --amend --no-edit": true,
		"git reflog expire --all":      true,
		"git gc --prune=now":           true,
		"shred secret":                 true,
		"echo 'unterminated":           true,
	} {
		if got := len(Dangerous(cmd)) > 0; got != want {
			t.Errorf("%q: dangerous=%v %v", cmd, got, Dangerous(cmd))
		}
	}
}
