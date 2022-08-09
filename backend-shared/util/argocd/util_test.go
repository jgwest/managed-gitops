package argocd

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Test Argo CD utility functions", func() {

	Context("Test ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId", func() {

		When("the cluster secret name starts with the managed env prefix", func() {

			It("should return the value for after the managed env prefix", func() {
				name, isLocal, err := ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId(managedEnvPrefix + "1234")
				Expect(name).To(Equal("1234"))
				Expect(err).To(BeNil())
				Expect(isLocal).To(BeFalse())
			})

			It("should return another value for after the managed env prefix", func() {
				name, isLocal, err := ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId(managedEnvPrefix + "1234-5678-910112")
				Expect(name).To(Equal("1234-5678-910112"))
				Expect(err).To(BeNil())
				Expect(isLocal).To(BeFalse())
			})

		})

		When("the cluster secret name is local", func() {
			It("should return isLocal to be true", func() {
				name, isLocal, err := ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId(ArgoCDDefaultDestinationInCluster)
				Expect(name).To(Equal(""))
				Expect(err).To(BeNil())
				Expect(isLocal).To(BeTrue())
			})
		})

		When("the cluster secret name does not start with managed env prefix, and is not a local secret", func() {

			It("should should return an error", func() {

				name, isLocal, err := ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId("just-a-random-secret-name")
				Expect(name).To(Equal(""))
				Expect(err).ToNot(BeNil())
				Expect(isLocal).To(BeFalse())

			})
		})
	})
})
