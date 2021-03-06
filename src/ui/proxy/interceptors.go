package proxy

import (
	"encoding/json"

	"github.com/vmware/harbor/src/common/dao"
	"github.com/vmware/harbor/src/common/models"
	"github.com/vmware/harbor/src/common/utils/clair"
	"github.com/vmware/harbor/src/common/utils/log"
	"github.com/vmware/harbor/src/common/utils/notary"
	//	"github.com/vmware/harbor/src/ui/api"
	"github.com/vmware/harbor/src/ui/config"
	"github.com/vmware/harbor/src/ui/projectmanager"

	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
)

type contextKey string

const (
	manifestURLPattern = `^/v2/((?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)+)manifests/([\w][\w.:-]{0,127})`
	imageInfoCtxKey    = contextKey("ImageInfo")
	//TODO: temp solution, remove after vmware/harbor#2242 is resolved.
	tokenUsername = "harbor-ui"
)

// Record the docker deamon raw response.
var rec *httptest.ResponseRecorder

// NotaryEndpoint , exported for testing.
var NotaryEndpoint = config.InternalNotaryEndpoint()

// EnvChecker is the instance of envPolicyChecker
var EnvChecker = envPolicyChecker{}

// MatchPullManifest checks if the request looks like a request to pull manifest.  If it is returns the image and tag/sha256 digest as 2nd and 3rd return values
func MatchPullManifest(req *http.Request) (bool, string, string) {
	//TODO: add user agent check.
	if req.Method != http.MethodGet {
		return false, "", ""
	}
	re := regexp.MustCompile(manifestURLPattern)
	s := re.FindStringSubmatch(req.URL.Path)
	if len(s) == 3 {
		s[1] = strings.TrimSuffix(s[1], "/")
		return true, s[1], s[2]
	}
	return false, "", ""
}

// policyChecker checks the policy of a project by project name, to determine if it's needed to check the image's status under this project.
type policyChecker interface {
	// contentTrustEnabled returns whether a project has enabled content trust.
	contentTrustEnabled(name string) bool
	// vulnerablePolicy  returns whether a project has enabled vulnerable, and the project's severity.
	vulnerablePolicy(name string) (bool, models.Severity)
}

//For testing
type envPolicyChecker struct{}

func (ec envPolicyChecker) contentTrustEnabled(name string) bool {
	return os.Getenv("PROJECT_CONTENT_TRUST") == "1"
}
func (ec envPolicyChecker) vulnerablePolicy(name string) (bool, models.Severity) {
	return os.Getenv("PROJECT_VULNERABLE") == "1", clair.ParseClairSev(os.Getenv("PROJECT_SEVERITY"))
}

type pmsPolicyChecker struct {
	pm projectmanager.ProjectManager
}

func (pc pmsPolicyChecker) contentTrustEnabled(name string) bool {
	project, err := pc.pm.Get(name)
	if err != nil {
		log.Errorf("Unexpected error when getting the project, error: %v", err)
		return true
	}
	return project.EnableContentTrust
}
func (pc pmsPolicyChecker) vulnerablePolicy(name string) (bool, models.Severity) {
	project, err := pc.pm.Get(name)
	if err != nil {
		log.Errorf("Unexpected error when getting the project, error: %v", err)
		return true, models.SevUnknown
	}
	return project.PreventVulnerableImagesFromRunning, clair.ParseClairSev(project.PreventVulnerableImagesFromRunningSeverity)
}

// newPMSPolicyChecker returns an instance of an pmsPolicyChecker
func newPMSPolicyChecker(pm projectmanager.ProjectManager) policyChecker {
	return &pmsPolicyChecker{
		pm: pm,
	}
}

// TODO: Get project manager with PM factory.
func getPolicyChecker() policyChecker {
	if config.WithAdmiral() {
		return newPMSPolicyChecker(config.GlobalProjectMgr)
	}
	return EnvChecker
}

type imageInfo struct {
	repository  string
	tag         string
	projectName string
	digest      string
}

type urlHandler struct {
	next http.Handler
}

//TODO: wrap a ResponseWriter to get the status code?

func (uh urlHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	log.Debugf("in url handler, path: %s", req.URL.Path)
	req.URL.Path = strings.TrimPrefix(req.URL.Path, RegistryProxyPrefix)
	flag, repository, tag := MatchPullManifest(req)
	if flag {
		components := strings.SplitN(repository, "/", 2)
		if len(components) < 2 {
			http.Error(rw, marshalError(fmt.Sprintf("Bad repository name: %s", repository), http.StatusInternalServerError), http.StatusBadRequest)
			return
		}
		rec = httptest.NewRecorder()
		uh.next.ServeHTTP(rec, req)
		if rec.Result().StatusCode != http.StatusOK {
			copyResp(rec, rw)
			return
		}
		digest := rec.Header().Get(http.CanonicalHeaderKey("Docker-Content-Digest"))
		img := imageInfo{
			repository:  repository,
			tag:         tag,
			projectName: components[0],
			digest:      digest,
		}
		log.Debugf("image info of the request: %#v", img)
		ctx := context.WithValue(req.Context(), imageInfoCtxKey, img)
		req = req.WithContext(ctx)
	}
	uh.next.ServeHTTP(rw, req)
}

type contentTrustHandler struct {
	next http.Handler
}

func (cth contentTrustHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	imgRaw := req.Context().Value(imageInfoCtxKey)
	if imgRaw == nil || !config.WithNotary() {
		cth.next.ServeHTTP(rw, req)
		return
	}
	img, _ := req.Context().Value(imageInfoCtxKey).(imageInfo)
	if !getPolicyChecker().contentTrustEnabled(img.projectName) {
		cth.next.ServeHTTP(rw, req)
		return
	}
	match, err := matchNotaryDigest(img)
	if err != nil {
		http.Error(rw, marshalError("Failed in communication with Notary please check the log", http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if !match {
		log.Debugf("digest mismatch, failing the response.")
		http.Error(rw, marshalError("The image is not signed in Notary.", http.StatusPreconditionFailed), http.StatusPreconditionFailed)
		return
	}
	cth.next.ServeHTTP(rw, req)
}

type vulnerableHandler struct {
	next http.Handler
}

func (vh vulnerableHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	imgRaw := req.Context().Value(imageInfoCtxKey)
	if imgRaw == nil || !config.WithClair() {
		vh.next.ServeHTTP(rw, req)
		return
	}
	img, _ := req.Context().Value(imageInfoCtxKey).(imageInfo)
	projectVulnerableEnabled, projectVulnerableSeverity := getPolicyChecker().vulnerablePolicy(img.projectName)
	if !projectVulnerableEnabled {
		vh.next.ServeHTTP(rw, req)
		return
	}
	overview, err := dao.GetImgScanOverview(img.digest)
	if err != nil {
		log.Errorf("failed to get ImgScanOverview with repo: %s, tag: %s, digest: %s. Error: %v", img.repository, img.tag, img.digest, err)
		http.Error(rw, marshalError("Failed to get ImgScanOverview.", http.StatusPreconditionFailed), http.StatusPreconditionFailed)
		return
	}
	if overview == nil {
		log.Debugf("cannot get the image scan overview info, failing the response.")
		http.Error(rw, marshalError("Cannot get the image scan overview info.", http.StatusPreconditionFailed), http.StatusPreconditionFailed)
		return
	}
	imageSev := overview.Sev
	if imageSev >= int(projectVulnerableSeverity) {
		log.Debugf("the image severity: %q is higher then project setting: %q, failing the response.", models.Severity(imageSev), projectVulnerableSeverity)
		http.Error(rw, marshalError(fmt.Sprintf("The severity of vulnerability of the image: %q is equal or higher than the threshold in project setting: %q.", models.Severity(imageSev), projectVulnerableSeverity),
			http.StatusPreconditionFailed), http.StatusPreconditionFailed)
		return
	}
	vh.next.ServeHTTP(rw, req)
}

type funnelHandler struct {
	next http.Handler
}

func (fu funnelHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	imgRaw := req.Context().Value(imageInfoCtxKey)
	if imgRaw != nil {
		log.Debugf("Return the original response as no the interceptor takes action.")
		copyResp(rec, rw)
		return
	}
	fu.next.ServeHTTP(rw, req)
}

func matchNotaryDigest(img imageInfo) (bool, error) {
	targets, err := notary.GetInternalTargets(NotaryEndpoint, tokenUsername, img.repository)
	if err != nil {
		return false, err
	}
	for _, t := range targets {
		if t.Tag == img.tag {
			log.Debugf("found tag: %s in notary, try to match digest.", img.tag)
			d, err := notary.DigestFromTarget(t)
			if err != nil {
				return false, err
			}
			return img.digest == d, nil
		}
	}
	log.Debugf("image: %#v, not found in notary", img)
	return false, nil
}

func copyResp(rec *httptest.ResponseRecorder, rw http.ResponseWriter) {
	for k, v := range rec.Header() {
		rw.Header()[k] = v
	}
	rw.WriteHeader(rec.Result().StatusCode)
	rw.Write(rec.Body.Bytes())
}

func marshalError(msg string, statusCode int) string {
	je := &JSONError{
		Message: msg,
		Code:    statusCode,
		Details: msg,
	}
	str, err := json.Marshal(je)
	if err != nil {
		log.Debugf("failed to marshal json error, %v", err)
		return msg
	}
	return string(str)
}

// JSONError wraps a concrete Code and Message, it's readable for docker deamon.
type JSONError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Details string `json:"details,omitempty"`
}
