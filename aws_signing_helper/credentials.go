package aws_signing_helper

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"runtime"
        "time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
        awscredentials "github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
        "github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/rolesanywhere-credential-helper/rolesanywhere"
)

type CredentialsOpts struct {
	PrivateKeyId        string
	CertificateId       string
	CertificateBundleId string
	CertIdentifier      CertIdentifier
	RoleArn             []string
	ProfileArnStr       string
	TrustAnchorArnStr   string
	SessionDuration     int
	Region              string
	Endpoint            string
	NoVerifySSL         bool
	WithProxy           bool
	Debug               bool
	Version             string
	LibPkcs11           string
	ReusePin            bool
}

// Function to create session and generate credentials
func GenerateCredentials(opts *CredentialsOpts, signer Signer, signatureAlgorithm string) (CredentialProcessOutput, error) {
	// Assign values to region and endpoint if they haven't already been assigned
	trustAnchorArn, err := arn.Parse(opts.TrustAnchorArnStr)
	if err != nil {
		return CredentialProcessOutput{}, err
	}
	profileArn, err := arn.Parse(opts.ProfileArnStr)
	if err != nil {
		return CredentialProcessOutput{}, err
	}

	if trustAnchorArn.Region != profileArn.Region {
		return CredentialProcessOutput{}, errors.New("trust anchor and profile regions don't match")
	}

	if opts.Region == "" {
		opts.Region = trustAnchorArn.Region
	}

	mySession := session.Must(session.NewSession())

	var logLevel aws.LogLevelType
	if Debug {
		logLevel = aws.LogDebug
	} else {
		logLevel = aws.LogOff
	}

	var tr *http.Transport
	if opts.WithProxy {
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.NoVerifySSL},
			Proxy:           http.ProxyFromEnvironment,
		}
	} else {
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.NoVerifySSL},
		}
	}
	client := &http.Client{Transport: tr}
	config := aws.NewConfig().WithRegion(opts.Region).WithHTTPClient(client).WithLogLevel(logLevel)
	if opts.Endpoint != "" {
		config.WithEndpoint(opts.Endpoint)
	}
	rolesAnywhereClient := rolesanywhere.New(mySession, config)
	rolesAnywhereClient.Handlers.Build.RemoveByName("core.SDKVersionUserAgentHandler")
	rolesAnywhereClient.Handlers.Build.PushBackNamed(request.NamedHandler{Name: "v4x509.CredHelperUserAgentHandler", Fn: request.MakeAddToUserAgentHandler("CredHelper", opts.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)})
	rolesAnywhereClient.Handlers.Sign.Clear()
	certificate, err := signer.Certificate()
	if err != nil {
		return CredentialProcessOutput{}, errors.New("unable to find certificate")
	}
	certificateChain, err := signer.CertificateChain()
	if err != nil {
		// If the chain couldn't be found, don't include it in the request
		if Debug {
			log.Println(err)
		}
	}
	rolesAnywhereClient.Handlers.Sign.PushBackNamed(request.NamedHandler{Name: "v4x509.SignRequestHandler", Fn: CreateRequestSignFunction(signer, signatureAlgorithm, certificate, certificateChain)})

	certificateStr := base64.StdEncoding.EncodeToString(certificate.Raw)
	durationSeconds := int64(opts.SessionDuration)
        var firstRoleArn string
        var remainingRoleArns []string
        firstRoleArn, remainingRoleArns = opts.RoleArn[0], opts.RoleArn[1:]
	createSessionRequest := rolesanywhere.CreateSessionInput{
		Cert:               &certificateStr,
		ProfileArn:         &opts.ProfileArnStr,
		TrustAnchorArn:     &opts.TrustAnchorArnStr,
		DurationSeconds:    &(durationSeconds),
		InstanceProperties: nil,
		RoleArn:            &firstRoleArn,
		SessionName:        nil,
	}
	output, err := rolesAnywhereClient.CreateSession(&createSessionRequest)
	if err != nil {
		return CredentialProcessOutput{}, err
	}

	if len(output.CredentialSet) == 0 {
		msg := "unable to obtain temporary security credentials from CreateSession"
		return CredentialProcessOutput{}, errors.New(msg)
	}
	credentials := output.CredentialSet[0].Credentials
        var currentRoleArn = firstRoleArn
        for i := 0; i < len(remainingRoleArns); i++ {
	    if Debug {
		log.Printf("using %s to assume %s\n",currentRoleArn,remainingRoleArns[i])
	    }

	    sess, err := session.NewSession(&aws.Config{
		Region:      &opts.Region,
	        Credentials: awscredentials.NewStaticCredentials(
                    *credentials.AccessKeyId,
                    *credentials.SecretAccessKey,
                    *credentials.SessionToken,
                ),
	    })
            stsClient := sts.New(sess)
            stsRequest := sts.AssumeRoleInput{
                RoleArn:         aws.String(remainingRoleArns[i]),
                RoleSessionName: aws.String("my-role-test"),
                DurationSeconds: aws.Int64(durationSeconds), //min allowed
            }

            stsResponse, err := stsClient.AssumeRole(&stsRequest)
            if err != nil {
                return CredentialProcessOutput{}, err
	    }
            xp := stsResponse.Credentials.Expiration.Format(time.RFC3339)
            credentials = &rolesanywhere.Credentials{
                AccessKeyId:         stsResponse.Credentials.AccessKeyId,
                SecretAccessKey:     stsResponse.Credentials.SecretAccessKey,
                SessionToken:        stsResponse.Credentials.SessionToken,
                Expiration:          &xp,
            }

            currentRoleArn = remainingRoleArns[i]
        }
	credentialProcessOutput := CredentialProcessOutput{
		Version:         1,
		AccessKeyId:     *credentials.AccessKeyId,
		SecretAccessKey: *credentials.SecretAccessKey,
		SessionToken:    *credentials.SessionToken,
		Expiration:      *credentials.Expiration,
	}
	return credentialProcessOutput, nil
}
