package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type API struct {
	host        string
	user        string
	bearerToken string
	pass        string
	client      http.Client
	pageSize    int
}

type manifestVersion uint

const wwwAuthenticateHeader = "Www-Authenticate"

const (
	manifestV1 manifestVersion = 1
	manifestV2 manifestVersion = 2
)

var manifestContentType = map[manifestVersion]string{
	manifestV1: "application/vnd.docker.distribution.manifest.v1+prettyjws",
	manifestV2: "application/vnd.docker.distribution.manifest.v2+json",
}

func NewAPI(host string) *API {
	return &API{
		host:   host,
		client: http.Client{},
	}
}

// SetHTTPClient changes the http.Client used for http requests.
func (a *API) SetHTTPClient(client *http.Client) {
	a.client = *client
}

// SetPageSize overrides the default page size used by the API.
func (a *API) SetPageSize(size int) {
	a.pageSize = size
}

// SetCredentials sets basic auth credentials used for communication with the registry HTTP API.
func (a *API) SetCredentials(user string, pass string) {
	a.user = user
	a.pass = pass
}

// GetRepositories returns all repository names of the registry.
func (a *API) GetRepositories() ([]string, error) {
	var repositories []string

	path := "/v2/_catalog"
	for path != "" {
		resp, err := a.request("GET", path, manifestV2)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var catalog struct {
			Repositories []string `json:"repositories"`
		}

		err = json.NewDecoder(resp.Body).Decode(&catalog)
		if err != nil {
			return nil, err
		}

		repositories = append(repositories, catalog.Repositories...)
		path = a.nextPagePath(resp)

		err = resp.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	return repositories, nil
}

// GetTagsIndexedByDigest returns a "digest => tag slice" map containing all tags of the given repository.
func (a *API) GetTagsIndexedByDigest(repository string) (map[string][]string, error) {
	tags, err := a.GetTags(repository)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]string)
	for _, tag := range tags {
		digest, err := a.GetDigest(repository, tag)
		if err != nil {
			return nil, err
		}

		_, exists := result[digest]
		if !exists {
			result[digest] = []string{}
		}

		result[digest] = append(result[digest], tag)
	}

	return result, nil
}

// GetTags returns all tags of the given repository.
func (a *API) GetTags(repository string) ([]string, error) {
	resp, err := a.request("GET", "/v2/"+repository+"/tags/list", manifestV2)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var tagList struct {
		Tags []string `json:"tags"`
	}

	err = json.NewDecoder(resp.Body).Decode(&tagList)
	if err != nil {
		return nil, err
	}

	return tagList.Tags, nil
}

// GetManifestCreated returns the creation time of the manifest referenced by the given tag in the given repository.
func (a *API) GetManifestCreated(repository string, tag string) (time.Time, error) {
	_, t, err := a.GetManifestDigestAndCreated(repository, tag)
	return t, err
}

// GetManifestDigestAndCreated returns the digest and creation time.
func (a *API) GetManifestDigestAndCreated(repository string, tag string) (string, time.Time, error) {
	resp, err := a.request("GET", "/v2/"+repository+"/manifests/"+tag, manifestV1)
	if err != nil {
		return "", time.Time{}, err
	}

	defer resp.Body.Close()

	// The v1 manifest contains json-encoded strings in the v1Compatibility property of each item in its history.
	// Unfortunately, they are the ones that hold the info we want, so first we have to get that json-encoded string out
	// from within the json-encoded manifest itself.

	var manifest struct {
		History []struct {
			V1Compatibility string `json:"v1Compatibility"`
		} `json:"history"`
	}

	err = json.NewDecoder(resp.Body).Decode(&manifest)
	if err != nil {
		return "", time.Time{}, err
	}

	// ... now we can get the timestamp from the first (thus newest) json-encoded history record we just extracted.

	var historyItem struct {
		Created time.Time `json:"created"`
	}

	err = json.Unmarshal([]byte(manifest.History[0].V1Compatibility), &historyItem)
	if err != nil {
		return "", time.Time{}, err
	}

	digest := resp.Header.Get("Docker-Content-Digest")

	return digest, historyItem.Created, nil
}

// GetDigest returns the digest of the given tag in the given repository.
func (a *API) GetDigest(repository string, tag string) (string, error) {
	resp, err := a.request("HEAD", "/v2/"+repository+"/manifests/"+tag, manifestV2)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	return resp.Header.Get("Docker-Content-Digest"), nil
}

// DeleteManifest deletes the given digest from the given repository.
func (a *API) DeleteManifest(repository string, digest string) error {
	resp, err := a.request("DELETE", "/v2/"+repository+"/manifests/"+digest, manifestV2)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	return nil
}

func (a *API) request(method string, path string, version manifestVersion) (resp *http.Response, err error) {

	resp, err = a.registryRequest(method, path, version)
	if err != nil {
		return resp, err
	}

	if resp.StatusCode < 300 {
		return resp, nil
	}

	defer func() {
		if err != nil && resp != nil {

			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if resp.StatusCode == 401 && resp.Header.Get(wwwAuthenticateHeader) != "" {
		token, err := a.fetchBearerToken(resp)
		if err != nil {
			return resp, err
		}

		a.bearerToken = token

		resp, err = a.registryRequest(method, path, version)
		if err != nil {
			return resp, err
		}
	}

	if resp.StatusCode >= 300 {
		return resp, fmt.Errorf("Got non-success HTTP status %d when sending %s %s.", resp.StatusCode, method, path)
	}

	return resp, nil
}

func (a *API) registryRequest(method string, path string, version manifestVersion) (*http.Response, error) {
	req, err := http.NewRequest(method, a.host+path, nil)
	if err != nil {
		return nil, err
	}

	if a.pageSize > 0 {
		q := req.URL.Query()
		q.Set("n", strconv.Itoa(a.pageSize))
		req.URL.RawQuery = q.Encode()
	}

	contentType, found := manifestContentType[version]
	if !found {
		panic(fmt.Sprintf("Invalid manifestVersion '%d'.", version))
	}

	req.Header.Add("Accept", contentType)

	if a.user != "" {
		req.SetBasicAuth(a.user, a.pass)
	}
	if a.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.bearerToken)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (a *API) fetchBearerToken(deniedResponse *http.Response) (string, error) {

	u, err := tokenRequestURLFromResponse(deniedResponse)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}

	if a.user != "" {
		req.SetBasicAuth(a.user, a.pass)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected response code %d from token service", resp.StatusCode)
	}

	var t struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&t)
	return t.Token, err
}

func (a *API) nextPagePath(resp *http.Response) string {
	link := resp.Header.Get("Link")
	if link == "" {
		return ""
	}

	begin := strings.Index(link, "<") + 1
	end := strings.LastIndex(link, ">")

	return link[begin:end]
}

func tokenRequestURLFromResponse(deniedResponse *http.Response) (string, error) {
	header := deniedResponse.Header.Get(wwwAuthenticateHeader)
	if header[:7] != "Bearer " {
		return "", fmt.Errorf(`%s response header refers to unsupported authentication scheme`, wwwAuthenticateHeader)
	}

	instructions := extractKeyValuePairs(header)
	realm, present := instructions["realm"]
	if !present {
		return "", fmt.Errorf(`no realm found in %s response header`, wwwAuthenticateHeader)
	}
	service, present := instructions["service"]
	if !present {
		return "", fmt.Errorf(`no service found in %s response header`, wwwAuthenticateHeader)
	}
	scope, present := instructions["scope"]
	if !present {
		return "", fmt.Errorf(`no scope found in %s response header`, wwwAuthenticateHeader)
	}

	params := url.Values{}
	params.Set("service", service)
	params.Set("scope", scope)

	return fmt.Sprintf("%s?%s", realm, params.Encode()), nil
}

func extractKeyValuePairs(header string) map[string]string {
	re := regexp.MustCompile(`(\w+)=\"([^\"]*)\"`)
	ms := re.FindAllSubmatch([]byte(header), -1)
	result := make(map[string]string, len(ms))

	for _, m := range ms {
		result[string(m[1])] = string(m[2])
	}
	return result
}
