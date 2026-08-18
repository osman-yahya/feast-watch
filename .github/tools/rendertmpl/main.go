// Command rendertmpl renders a Go text/template shell script with placeholder
// data so CI can syntax-check and lint what agents actually receive. The
// template is not valid shell until rendered, so linting the raw file is not
// an option. It lives under .github/, which the go tool excludes from ./...,
// so it never ships in a build of the mother or the agent.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"github.com/osman-yahya/feast-watch/shared/release"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: rendertmpl <template> <out>")
		os.Exit(2)
	}
	// missingkey=error mirrors the production renderer: a field CI forgets to
	// supply must fail here rather than lint a script containing "<no value>".
	tmpl, err := template.New(filepath.Base(os.Args[1])).
		Option("missingkey=error").ParseFiles(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer out.Close()
	if err := tmpl.Execute(out, map[string]any{
		"MotherURL":      "http://127.0.0.1:8443",
		"Token":          "tk_placeholder",
		"ServerName":     "placeholder",
		"ReleaseBaseURL": release.DefaultBaseURL,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
