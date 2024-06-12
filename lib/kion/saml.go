package kion

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	saml2 "github.com/russellhaering/gosaml2"
	samlTypes "github.com/russellhaering/gosaml2/types"
	dsig "github.com/russellhaering/goxmldsig"
)

var (
	// SAMLLocalAuthPort is the port to use to accept back the access token from SAML
	SAMLLocalAuthPort = "8400"
)

type CSRFResponse struct {
	Data string `json:"data"`
}

type SSOAuthResponse struct {
	Data AccessData `json:"data"`
}

type AccessData struct {
	Access TokenData `json:"access"`
}

type TokenData struct {
	Token string `json:"token"`
}

type AuthData struct {
	AuthToken string
	Cookies   []*http.Cookie
	CSRFToken string
}

type SamlCallbackResult struct {
	Data *AuthData
	Err  error
}

func AuthenticateSAML(appUrl string, metadata *samlTypes.EntityDescriptor, serviceProviderIssuer string) (*AuthData, error) {
	certStore := dsig.MemoryX509CertificateStore{
		Roots: []*x509.Certificate{},
	}

	for _, kd := range metadata.IDPSSODescriptor.KeyDescriptors {
		for idx, xcert := range kd.KeyInfo.X509Data.X509Certificates {
			if xcert.Data == "" {
				return nil, fmt.Errorf("metadata certificate(%d) must not be empty", idx)
			}
			certData, err := base64.StdEncoding.DecodeString(xcert.Data)
			if err != nil {
				return nil, err
			}

			idpCert, err := x509.ParseCertificate(certData)
			if err != nil {
				return nil, err
			}

			certStore.Roots = append(certStore.Roots, idpCert)
		}
	}

	// TODO: Allow importing private key and certificate from Kion application
	// For now we use a generated key/cert to sign the request, which will work
	// unless the customer has set up the IDP to verify our SP cert.
	randomKeyStore := dsig.RandomKeyStoreForTest()

	sp := &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL:      metadata.IDPSSODescriptor.SingleSignOnServices[0].Location,
		IdentityProviderIssuer:      metadata.EntityID,
		ServiceProviderIssuer:       serviceProviderIssuer,
		AssertionConsumerServiceURL: "http://localhost:" + SAMLLocalAuthPort + "/callback",
		SignAuthnRequests:           false,
		IDPCertificateStore:         &certStore,
		SPKeyStore:                  randomKeyStore,
	}

	tokenChan := make(chan SamlCallbackResult, 1)
	http.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.String(), "/favicon.ico") {
			http.NotFound(rw, req)
			return
		}

		// Ensure we work with private network access check preflight requests
		if req.Method == "OPTIONS" {
			return
		}

		b, err := io.ReadAll(req.Body)
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("bad SAML callback request: %w", err)}
			return
		}

		client := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		jar, err := cookiejar.New(nil)
		if err != nil {
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("failed to create an empty cookie jar: %w", err)}
			return
		}
		client.Jar = jar

		r, err := http.NewRequest("POST", appUrl+"/api/v1/saml/callback", bytes.NewReader(b))
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("error creating SAML request: %w", err)}
			return
		}
		r.Header.Add("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(r)
		if err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("error posting SAML assertion: %w", err)}
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("error reading SAML response body: %w", err)}
			return
		}

		ssoCodeRegexp, err := regexp.Compile(`token: '(.+)',`)
		if err != nil {
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("failed to compile access token regular expression: %w", err)}
			return
		}
		groups := ssoCodeRegexp.FindStringSubmatch(string(body))
		if len(groups) < 2 {
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("could not find SSO code in SAML authentication response.  Response: %v", string(body))}
			return
		}
		// parse the sso code from the groups
		ssoCode := groups[1]

		// send auto-close response before returning token
		_, err = rw.Write([]byte(`
		<!doctype html>
		<html lang="en">
		  <head>
        <meta charset="utf-8">
        <title>Kion-CLI</title>
        <style>
          html {
            background: #f3f7f4;
          }
          body {
            display: flex;
            justify-content: center;
            align-items: center;
            height: 100vh;
            margin: 0;
          }
          #wrapper {
            text-align: center;
            font-family: monospace, monospace;
          }
        </style>
		  </head>
		  <body>
        <div id="wrapper">
          <svg class="kion_logo_mark" viewBox="0 0 500.00001 499.99998" version="1.1" width="150" height="150" xmlns="http://www.w3.org/2000/svg" xmlns:svg="http://www.w3.org/2000/svg">
            <path id="logoMark" d="m 99.882574,277.61145 -57.26164,71.71925 -7.378755,-19.96374 a 228.4366,228.4366 0 0 1 -8.809416,-30.09222 l -1.227632,-5.59757 32.199414,-40.32547 a 3.7941326,3.7941326 0 0 0 0.01752,-4.71222 L 25,207.40537 l 1.18086,-5.51016 a 228.0104,228.0104 0 0 1 8.737594,-30.39825 l 7.395922,-20.26924 57.785764,73.49185 a 41.908883,41.908883 0 0 1 -0.217566,52.89188 z M 350.42408,252.5466 a 9.7816414,9.7816414 0 0 1 0.0175,-6.9699 L 411.27297,87.263147 405.28196,81.733373 A 231.43333,231.43333 0 0 0 384.39067,64.61169 L 371.72774,55.418289 305.32087,228.24236 a 58.091098,58.091098 0 0 0 -0.10371,41.41155 l 66.25377,175.08822 12.72442,-9.21548 a 230.66081,230.66081 0 0 0 20.93859,-17.12659 l 5.96806,-5.49911 -60.67792,-160.35313 z m 92.26509,-5.157 L 475,206.92118 l -1.20766,-5.57917 a 228.10814,228.10814 0 0 0 -8.73777,-30.17859 l -7.35283,-20.04081 -57.4913,72.00601 a 41.902051,41.902051 0 0 0 -0.22002,52.89399 l 57.56049,73.20281 7.42588,-20.18989 a 228.3357,228.3357 0 0 0 8.80171,-30.31802 l 1.19838,-5.5275 -32.30645,-41.08678 a 3.7946582,3.7946582 0 0 1 0.0175,-4.71363 z M 237.23179,21.415791 l -11.3535,0.62748 V 477.95476 l 11.3535,0.6273 c 4.35767,0.24104 8.6684,0.36332 12.81341,0.36332 4.14501,0 8.45591,-0.12263 12.81358,-0.36332 l 11.35349,-0.6273 V 22.043271 l -11.35349,-0.62748 a 227.47839,227.47839 0 0 0 -25.62699,0 z M 128.39244,55.397443 115.66276,64.640069 A 230.8761,230.8761 0 0 0 94.739412,81.801341 L 88.786063,87.300109 149.66684,248.1926 a 9.7721819,9.7721819 0 0 1 -0.0175,6.972 l -60.623967,157.77734 6.00853,5.52837 a 231.25886,231.25886 0 0 0 20.901277,17.08717 l 12.65785,9.16625 66.17459,-172.22251 a 58.03837,58.03837 0 0 0 0.10615,-41.41348 z" style="fill:#61d7ac;stroke-width:1.75176" />
          </svg>
          <p>YOU MAY CLOSE THIS WINDOW</p>
          <script type="text/javascript">
            window.close()
          </script>
        </div>
		  </body>
		</html>
		`))
		if err != nil {
			tokenChan <- SamlCallbackResult{Data: nil, Err: fmt.Errorf("failed to send auto-close response: %w", err)}
			return
		}

		tokenChan <- SamlCallbackResult{Data: &AuthData{
			AuthToken: ssoCode,
		}, Err: nil}
	})

	authURL, err := sp.BuildAuthURL("")
	if err != nil {
		log.Fatalf("The login info is invalid.\n %v", err)
	}
	var chromeCommand *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		chromeCommand = exec.Command("start", "chrome", authURL)
	case "darwin":
		chromeCommand = exec.Command("open", authURL)
	case "linux":
		chromeCommand = exec.Command("/usr/bin/google-chrome", "--new-window", authURL)
	}
	err = chromeCommand.Run()
	if chromeCommand == nil || err != nil {
		if err != nil {
			println("Error opening Chrome browser: ", err)
		} else {
			println("Could not locate Chrome browser")
		}
		println("Visit this URL To Authenticate:")
		println(authURL)
	}

	server := &http.Server{Addr: ":" + SAMLLocalAuthPort}

	go func() {

		tempResult := <-tokenChan
		err = server.Close()
		if err != nil {
			tokenChan <- SamlCallbackResult{Data: nil, Err: err}
			return
		}
		tokenChan <- tempResult
	}()

	err = server.ListenAndServe()
	if err != nil && !strings.Contains(fmt.Sprintf("%v", err), "Server closed") {
		log.Fatalf("The login info is invalid.\n %v", err)
	}

	samlResult := <-tokenChan

	if samlResult.Err != nil {
		return nil, samlResult.Err
	}

	return samlResult.Data, nil
}

func DownloadSAMLMetadata(metadataUrl string) (*samlTypes.EntityDescriptor, error) {
	res, err := http.Get(metadataUrl)
	if err != nil {
		return nil, fmt.Errorf("error downloading SAML metadata file from %v: %w", metadataUrl, err)
	}
	defer res.Body.Close()

	rawMetadata, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("error downloading SAML metadata file from %v: %w", metadataUrl, err)
	}

	metadata := &samlTypes.EntityDescriptor{}
	err = xml.Unmarshal(rawMetadata, metadata)
	if err != nil {
		return nil, fmt.Errorf("error parsing SAML metadata file from %v: %w", metadataUrl, err)
	}

	return metadata, nil
}

func ReadSAMLMetadataFile(metadataFile string) (*samlTypes.EntityDescriptor, error) {
	rawMetadata, err := os.ReadFile(metadataFile)
	if err != nil {
		return nil, fmt.Errorf("error reading SAML metadata file %v: %w", metadataFile, err)
	}

	metadata := &samlTypes.EntityDescriptor{}
	err = xml.Unmarshal(rawMetadata, metadata)
	if err != nil {
		return nil, fmt.Errorf("error parsing SAML metadata file %v: %w", metadataFile, err)
	}

	return metadata, nil
}
