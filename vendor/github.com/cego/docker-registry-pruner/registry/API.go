package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type API struct {
	host     string
	headers  map[string]string
	client   http.Client
	pageSize int
}

type manifestVersion uint

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
		host:    host,
		client:  http.Client{},
		headers: map[string]string{},
	}
}

func (a *API) SetClient(client *http.Client) {
	a.client = *client
}

func (a *API) SetPageSize(size int) {
	a.pageSize = size
}

// SetCredentials sets basic auth credentials used for communication with the registry HTTP API.
func (a *API) SetCredentials(user string, pass string) {
	a.headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// GetRepositories returns all repository names of the registry.
func (a *API) GetRepositories() ([]string, error) {
	var repositories []string

	path := "/v2/_catalog"
	for path != "" {
		resp, err := a.doRequest("GET", path, manifestV2)
		if err != nil {
			return nil, err
		}

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
	resp, err := a.doRequest("GET", "/v2/"+repository+"/tags/list", manifestV2)
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

func (a *API) GetManifestDigestAndCreated(repository string, tag string) (string, time.Time, error) {
	resp, err := a.doRequest("GET", "/v2/"+repository+"/manifests/"+tag, manifestV1)
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
	resp, err := a.doRequest("HEAD", "/v2/"+repository+"/manifests/"+tag, manifestV2)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	return resp.Header.Get("Docker-Content-Digest"), nil
}

// DeleteManifest deletes the given digest from the given repository.
func (a *API) DeleteManifest(repository string, digest string) error {
	resp, err := a.doRequest("DELETE", "/v2/"+repository+"/manifests/"+digest, manifestV2)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	return nil
}

func (a *API) doRequest(method string, path string, version manifestVersion) (*http.Response, error) {
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
	for key, value := range a.headers {
		req.Header.Add(key, value)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Got non-success HTTP status %d when sending %s %s.", resp.StatusCode, req.Method, req.URL.Path)
	}
	return resp, nil
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
