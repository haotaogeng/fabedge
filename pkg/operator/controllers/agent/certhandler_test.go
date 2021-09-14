// Copyright 2021 FabEdge Team
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2/klogr"

	certutil "github.com/fabedge/fabedge/pkg/util/cert"
	secretutil "github.com/fabedge/fabedge/pkg/util/secret"
	timeutil "github.com/fabedge/fabedge/pkg/util/time"
)

var _ = Describe("CertHandler", func() {
	var (
		namespace = "default"
		node      corev1.Node

		certManager certutil.Manager
		handler     *certHandler

		newNode = newNodePodCIDRsInAnnotations
	)

	BeforeEach(func() {
		caCertDER, caKeyDER, _ := certutil.NewSelfSignedCA(certutil.Config{
			CommonName:     certutil.DefaultCAName,
			Organization:   []string{certutil.DefaultOrganization},
			IsCA:           true,
			ValidityPeriod: timeutil.Days(365),
		})
		certManager, _ = certutil.NewManger(caCertDER, caKeyDER)
		handler = &certHandler{
			namespace:        namespace,
			client:           k8sClient,
			certManager:      certManager,
			certValidPeriod:  365,
			certOrganization: certutil.DefaultOrganization,
			log:              klogr.New().WithName("configHandler"),
		}

		nodeName := getNodeName()
		node = newNode(nodeName, "10.40.20.181", "2.2.1.128/26")

		Expect(handler.Do(context.Background(), node)).Should(Succeed())
	})

	It("should ensure a valid certificate and a private key for specified node's agent", func() {
		var secret corev1.Secret
		secretName := getCertSecretName(node.Name)
		Expect(k8sClient.Get(context.Background(), ObjectKey{Namespace: namespace, Name: secretName}, &secret)).Should(Succeed())

		By("Checking TLS secret")
		caCertPEM, certPEM := secretutil.GetCACert(secret), secretutil.GetCert(secret)
		Expect(certManager.VerifyCertInPEM(certPEM, certutil.ExtKeyUsagesServerAndClient)).Should(Succeed())
		Expect(caCertPEM).Should(Equal(certManager.GetCACertPEM()))

		By("Changing TLS secret with expired cert")
		certDER, keyDER, _ := certManager.SignCert(certutil.Config{
			CommonName:     node.Name,
			ValidityPeriod: time.Second,
		})
		secret.Data[corev1.TLSCertKey] = certutil.EncodeCertPEM(certDER)
		secret.Data[corev1.TLSPrivateKeyKey] = certutil.EncodePrivateKeyPEM(keyDER)
		Expect(k8sClient.Update(context.Background(), &secret)).Should(Succeed())

		time.Sleep(time.Second)

		Expect(handler.Do(context.Background(), node)).Should(Succeed())

		By("Checking if TLS secret updated")
		secret = corev1.Secret{}
		Expect(k8sClient.Get(context.Background(), ObjectKey{Namespace: namespace, Name: secretName}, &secret)).Should(Succeed())

		caCertPEM, certPEM = secretutil.GetCACert(secret), secretutil.GetCert(secret)
		Expect(certManager.VerifyCertInPEM(certPEM, certutil.ExtKeyUsagesServerAndClient)).Should(Succeed())
		Expect(caCertPEM).Should(Equal(certManager.GetCACertPEM()))
	})

	It("should be able to delete cert secret created for specified node", func() {
		Expect(handler.Undo(context.Background(), node.Name)).Should(Succeed())

		var secret corev1.Secret
		secretName := getCertSecretName(node.Name)
		err := k8sClient.Get(context.Background(), ObjectKey{Namespace: namespace, Name: secretName}, &secret)
		Expect(errors.IsNotFound(err)).Should(BeTrue())
	})
})
