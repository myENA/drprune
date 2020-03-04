package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cego/docker-registry-pruner/registry"
	"github.com/fsouza/go-dockerclient"
	"github.com/hashicorp/consul/api"
	"github.com/myENA/consul-decoder"
	"github.com/myENA/drprune/models"
	"github.com/rs/zerolog"
)

var log = zerolog.New(os.Stderr).With().Str("app", "drprune").Timestamp().Logger()

func main() {

	var registryURL string
	var user string
	var pass string
	var skipVerify bool
	var consulPrefix string
	var consulDefaultPath string
	var skipDeletes bool
	var runGC bool
	var drContainerName string
	var drConfigFilePath string

	fs := flag.NewFlagSet("drprune", flag.ExitOnError)
	fs.StringVar(
		&registryURL,
		"docker-registry-url",
		os.Getenv("DOCKER_REGISTRY_URL"),
		"pass this or DOCKER_REGISTRY_URL env - required",
	)
	fs.StringVar(
		&user,
		"docker-registry-user",
		os.Getenv("DOCKER_REGISTRY_USER"),
		"pass this or DOCKER_REGISTRY_USER env - optional",
	)
	fs.StringVar(
		&pass,
		"docker-registry-pass",
		os.Getenv("DOCKER_REGISTRY_PASS"),
		"pass this or DOCKER_REGISTRY_PASS env - optional",
	)
	fs.BoolVar(
		&skipVerify,
		"https-skip-verify",

		false,
		"ignore https errors - good for self-signed keys",
	)
	fs.StringVar(
		&consulDefaultPath,
		"consul-default-path",
		"apps/drprune/default",
		"path to default config",
	)
	fs.StringVar(
		&consulPrefix,
		"consul-prefix",
		"apps/drprune/overrides",
		"prefix for image based config overrides",
	)
	fs.BoolVar(
		&runGC,
		"run-gc",
		false,
		"run garage collector",
	)
	fs.StringVar(
		&drContainerName,
		"docker-registry-container-name-prefix",
		"docker-registry",
		"docker registry container name prefix",
	)
	fs.BoolVar(
		&skipDeletes,
		"skip-deletes",
		false,
		"this skips finding containers.  this can be useful if you just want to trigger gc",
	)
	fs.StringVar(
		&drConfigFilePath,
		"docker-registry-config-file-path",
		"/etc/docker/registry/config.yml",
		"docker registry config file path inside container",
	)
	_ = fs.Parse(os.Args[1:])

	if registryURL == "" {
		_, _ = fmt.Fprintln(os.Stderr, "must specify docker registry url")
		fs.Usage()
		os.Exit(1)
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   60 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          1,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	var (
		reg *registry.API
	)

	if skipVerify {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	reg = registry.NewAPI(registryURL)

	reg.SetHTTPClient(&http.Client{Transport: transport})

	reg.SetPageSize(100000)
	if !skipDeletes {
		cnl, err := api.NewClient(api.DefaultConfig())

		if err != nil {
			log.Fatal().Err(err).Msg("could not connect to consul")
		}

		cfg := models.DefaultConfig()

		d := &decoder.Decoder{
			CaseSensitive: false,
		}

		kvps, _, err := cnl.KV().List(consulDefaultPath, nil)
		fmt.Printf("kvps: %#v\n", kvps)
		if err != nil {
			log.Error().Err(err).Msgf("default config could not be read from config")
		} else {
			err = d.Unmarshal(consulDefaultPath, kvps, cfg)
			if err != nil {
				log.Error().Err(err).Msgf("could not unmarshal into config")
			}
		}

		fmt.Printf("config: %#v\n", cfg)

		repos, err := reg.GetRepositories()
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to list repositories from registry")
		}

		var tagsToDelete tagsMetaList
		for _, repo := range repos {
			p := path.Join(consulPrefix, repo)
			kvs, _, err := cnl.KV().List(p, nil)
			c := cfg
			if err != nil {
				log.Error().Err(err).Str("path", p).Msg("error fetching consul path config")
			} else if kvs != nil {
				// only allocate new config if present
				c = cfg.Clone()
				err = d.Unmarshal(p, kvps, c)
				if err != nil {
					log.Error().Err(err).Str("path", p).Msg("failed to unmarshal config")
				}
				fmt.Printf("found config: %#v\n", c)
			}
			tags, err := reg.GetTags(repo)
			if err != nil {
				log.Error().Err(err).Str("repo", repo).Msg("error processing repository")
				continue
			}

			var releaseTags tagsMetaList
			for _, tag := range tags {
				created, err := reg.GetManifestCreated(repo, tag)
				if err != nil {
					log.Error().Err(err).Str("repo", repo).Str("tag", tag).Msg("could not get manifest created")
					continue
				}
				digest, err := reg.GetDigest(repo, tag)
				if err != nil {
					log.Error().Err(err).Str("repo", repo).Str("tag", tag).Msg("could not get digest")
					continue
				}
				var isRelease bool
				for _, rt := range c.ReleaseTags {
					if strings.HasPrefix(tag, rt) {
						isRelease = true
						break
					}
				}
				if isRelease {
					releaseTags = append(releaseTags, tagsMeta{
						created: created,
						repo:    repo,
						tag:     tag,
						digest:  digest,
					})
				} else {
					// not special
					d := time.Since(created)
					if d > time.Duration(int(time.Hour)*24*c.MinFeatureEvictionDays) {
						tagsToDelete = append(tagsToDelete, tagsMeta{
							created: created,
							repo:    repo,
							tag:     tag,
							digest:  digest,
						})
					}

				}
			}
			if len(releaseTags) > c.MinReleaseImages {
				sort.Sort(releaseTags)
				for _, rt := range releaseTags[c.MinReleaseImages:] {
					d := time.Since(rt.created)
					if d > time.Duration(int(time.Hour)*24*c.MinReleaseEvictionDays) {
						tagsToDelete = append(tagsToDelete, rt)
					}
				}
			}

		}

		sort.Sort(sort.Reverse(tagsToDelete))
		l := len(tagsToDelete)
		for i, t := range tagsToDelete {
			log.Info().
				Str("progress", fmt.Sprintf("%d of %d", i+1, l)).
				Str("repo", t.repo).Str("tag", t.tag).
				Time("created", t.created).
				Msg("deleting")
			err = reg.DeleteManifest(t.repo, t.digest)
			if err != nil {
				log.Error().Err(err).Str("repo", t.repo).Str("tag", t.tag).Msg("error deleting manifest")
			}
		}
	}
	if runGC {
		dc, err := docker.NewClientFromEnv()
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to initialize docker client")
		}
		containers, err := dc.ListContainers(docker.ListContainersOptions{})
		if err != nil {
			log.Fatal().Err(err).Msg("could not list containers")
		}
		for _, c := range containers {
			var found bool
			for _, n := range c.Names {
				if strings.HasPrefix(path.Base(n), drContainerName) {
					found = true
					break
				}
			}
			if found {
				var stdOutBuf bytes.Buffer
				var stdErrBuf bytes.Buffer
				exec, err := dc.CreateExec(docker.CreateExecOptions{
					Cmd:          []string{"/bin/registry", "garbage-collect", drConfigFilePath},
					Container:    c.ID,
					AttachStdout: true,
					AttachStderr: true,
				})
				if err != nil {
					log.Fatal().Err(err).Msg("failed to CreateExec")
				}
				err = dc.StartExec(exec.ID, docker.StartExecOptions{
					OutputStream: &stdOutBuf,
					ErrorStream:  &stdErrBuf,
				})

				if err != nil {
					log.Error().Err(err).Msg("error running command")
				}
				fmt.Printf("stdout: \n%s\n", stdOutBuf.String())
				fmt.Printf("stderr: \n%s\n", stdErrBuf.String())
				break
			}
		}
	}
}

type tagsMeta struct {
	created time.Time
	repo    string
	tag     string
	digest  string
}

type tagsMetaList []tagsMeta

func (t tagsMetaList) Len() int {
	return len(t)
}

func (t tagsMetaList) Less(i, j int) bool {
	return t[i].created.Before(t[j].created)
}

func (t tagsMetaList) Swap(i, j int) {
	t[j], t[i] = t[i], t[j]
}
