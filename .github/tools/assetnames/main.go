// Command assetnames prints every file a complete release must carry, one per
// line, so the release workflow can assert what it actually published.
//
// The list comes from shared/release rather than from the workflow YAML: the
// build matrix and the mother's platform list already drifted apart once, and
// an assertion written out by hand would be the third copy to keep in step.
//
// It lives under .github/, which the go tool excludes from ./..., so it never
// ships in a build of the mother or the agent.
package main

import (
	"fmt"

	"github.com/osman-yahya/feast-watch/shared/release"
)

func main() {
	for _, name := range release.ExpectedAssets() {
		fmt.Println(name)
	}
}
