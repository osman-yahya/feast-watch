// Command printversion prints the version compiled into the binary, so CI can
// prove -ldflags actually took. A build without it reports "dev" forever, and
// no agent can then satisfy any rollout target.
//
// It lives under .github/, which the go tool excludes from ./..., so it never
// ships in a build of the mother or the agent.
package main

import (
	"fmt"

	"github.com/osman-yahya/feast-watch/shared/version"
)

func main() { fmt.Println(version.Version) }
