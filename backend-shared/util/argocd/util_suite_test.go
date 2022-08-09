package argocd

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestEventlooptypes(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Eventlooptypes Suite")
}
