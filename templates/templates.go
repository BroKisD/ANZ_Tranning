// Package templates embeds the workflow templates committed into generated repos.
package templates

import _ "embed"

//go:embed deploy.yml.tmpl
var DeployWorkflow string
