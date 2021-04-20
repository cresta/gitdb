// +build mage

package main

import (
	_ "github.com/cresta/magehelper/cicd/githubactions"
	"github.com/cresta/magehelper/docker/registry"
	// mage:import go
	_ "github.com/cresta/magehelper/gobuild"
	// mage:import docker
	_ "github.com/cresta/magehelper/docker"
	// mage:import ghcr
	"github.com/cresta/magehelper/docker/registry/ghcr"
)

func init() {
	// Install ECR as my registry
	registry.Instance = ghcr.Instance
}
