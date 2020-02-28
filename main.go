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

var log = zerolog.New(&zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: ""}).With().Str("app", "drprune").Timestamp().Logger()

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
	var catalogPageSize int

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
	fs.IntVar(
		&catalogPageSize,
		"catalog-page-size",
		10000,
		"number of records to return per /v2/catalog api call",
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

	reg.SetPageSize(catalogPageSize)
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

		log.Info().Object("config", cfg).Msg("Config parsed")

		repos, err := reg.GetRepositories()
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to list repositories from registry")
		}

		log.Info().Msgf("Found %d repo configurations", len(repos))

		var tagsToDelete tagsMetaList
		for _, repo := range repos {
			log := log.With().Str("action", "delete-images").Str("repo", repo).Logger()

			log.Info().Msgf("Processing repo: %s", repo)

			p := path.Join(consulDefaultPath, repo)
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
				log.Info().Object("config", c).Msg("Found config")
			}
			tags, err := reg.GetTags(repo)
			if err != nil {
				log.Error().Err(err).Str("repo", repo).Msg("error processing repository")
				continue
			}

			var releaseTags tagsMetaList
			for _, tag := range tags {
				log := log.With().Str("tag", tag).Logger()
				log.Info().Msgf("Processing tag: %s", tag)

				digest, created, err := reg.GetManifestDigestAndCreated(repo, tag)
				if err != nil {
					log.Error().Err(err).Msg("could not get manifest created")
					continue
				}

				log = log.With().Str("manifest", digest).Time("created", created).Logger()

				log.Debug().Msg("Tag data found")

				var isRelease bool
				for _, rt := range c.ReleaseTags {
					if strings.HasPrefix(tag, rt) {
						isRelease = true
						break
					}
				}
				if isRelease {
					log.Debug().Msg("Tag identified as containing a release build")
					releaseTags = append(releaseTags, tagsMeta{
						created: created,
						repo:    repo,
						tag:     tag,
						digest:  digest,
					})
				} else {
					log.Debug().Msg("Tag identified as containing a development build")
					// not special
					d := time.Since(created)
					if d > time.Duration(int(time.Hour)*24*c.MinFeatureEvictionDays) {
						log.Info().Msg("Development tag is beyond purge threshold, adding to delete list...")
						tagsToDelete = append(tagsToDelete, tagsMeta{
							created: created,
							repo:    repo,
							tag:     tag,
							digest:  digest,
						})
					} else {
						log.Debug().Msg("Development tag is not beyond purge threshold, will not delete")
					}
				}
			}

			log.Debug().Msgf("Found %d release images", len(releaseTags))

			if len(releaseTags) > c.MinReleaseImages {
				log.Info().Msgf("Above configured release image purge threshold (%d > %d)", len(releaseTags), c.MinReleaseImages)
				sort.Sort(releaseTags)
				for _, rt := range releaseTags[c.MinReleaseImages:] {
					log := log.With().Str("tag", rt.tag).Logger()
					d := time.Since(rt.created)
					if d > time.Duration(int(time.Hour)*24*c.MinReleaseEvictionDays) {
						log.Info().Msg("Release tag is beyond purge threshold, adding to delete list...")
						tagsToDelete = append(tagsToDelete, rt)
					} else {
						log.Debug().Msg("Release tag is not beyond purge threshold, will not delete")
					}
				}
			} else {
				log.Debug().Msgf("Not above configured release image purge threshold (%d <= %d)", len(releaseTags), c.MinReleaseImages)
			}
		}

		log.Info().Msgf("Found %d tags to delete", len(tagsToDelete))

		sort.Sort(sort.Reverse(tagsToDelete))
		for _, t := range tagsToDelete {
			log := log.With().Str("tag", t.tag).Logger()
			log.Warn().Msg("Deleting tag...")
			err = reg.DeleteManifest(t.repo, t.digest)
			if err != nil {
				log.Error().Err(err).Str("repo", t.repo).Str("tag", t.tag).Msg("error deleting manifest")
			} else {
				log.Debug().Msg("Tag deleted successfully")
			}
		}
	}
	if runGC {
		log := log.With().Str("action", "gc").Logger()
		log.Warn().Msg("Running garbage collection...")
		dc, err := docker.NewClientFromEnv()
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to initialize docker client")
		}
		containers, err := dc.ListContainers(docker.ListContainersOptions{})
		if err != nil {
			log.Fatal().Err(err).Msg("could not list containers")
		}

		gcdContainers := make(map[string]error, 0)

		log.Info().Msgf("Found %d containers", len(containers))

	ContainerLoop:
		for _, c := range containers {
			log := log.With().Str("container-id", c.ID).Logger()
			var found bool
			log.Info().Msgf("Locating docker-registry container with prefix %q from %d entries...", drContainerName, len(c.Names))
			for _, n := range c.Names {
				if strings.HasPrefix(path.Base(n), drContainerName) {
					log.Info().Str("name", n).Msg("Registry container found")
					found = true
					break
				}
			}

			if !found {
				continue ContainerLoop
			}

			log.Warn().Msg("Running garbage collection on container")

			var stdOutBuf bytes.Buffer
			var stdErrBuf bytes.Buffer

			cmd := []string{"/bin/registry", "garbage-collect", drConfigFilePath}

			log = log.With().Str("cmd", strings.Join(cmd, " ")).Logger()

			log.Info().Msg("Calling CreateExec with args...")

			exec, err := dc.CreateExec(docker.CreateExecOptions{
				Cmd:          cmd,
				Container:    c.ID,
				AttachStdout: true,
				AttachStderr: true,
			})

			if err != nil {
				gcdContainers[c.ID] = err
				log.Error().Err(err).Msg("CreateExec call failed")
				continue ContainerLoop
			}
			err = dc.StartExec(exec.ID, docker.StartExecOptions{
				OutputStream: &stdOutBuf,
				ErrorStream:  &stdErrBuf,
			})

			if err != nil {
				gcdContainers[c.ID] = err
				log.Error().Err(err).Msg("error running command")
			} else {
				gcdContainers[c.ID] = nil
			}

			log.Info().Bytes("stdout", stdOutBuf.Bytes()).Bytes("stderr", stdErrBuf.Bytes()).Msg("Output")
		}

		if len(gcdContainers) == 0 {
			log.Warn().Msg("No docker registry containers were found to run garbage collection on")
		} else {
			dict := zerolog.Dict()
			for k, v := range gcdContainers {
				dict.AnErr(k, v)
			}
			log.Info().Dict("gc-results", dict).Msgf("Garbage collection was run on %d containers", len(gcdContainers))
		}
	}
}

type tagsMeta struct {
	created time.Time
	repo    string
	tag     string
	digest  string
}

func (t tagsMeta) MarshalZerologObject(ev *zerolog.Event) {
	ev.Time("created", t.created)
	ev.Str("repo", t.repo)
	ev.Str("tag", t.tag)
	ev.Str("digest", t.digest)
}

type tagsMetaList []tagsMeta

func (t tagsMetaList) MarshalZerologArray(ar *zerolog.Array) {
	for _, v := range t {
		ar.Object(v)
	}
}

func (t tagsMetaList) Len() int {
	return len(t)
}

func (t tagsMetaList) Less(i, j int) bool {
	return t[i].created.Before(t[j].created)
}

func (t tagsMetaList) Swap(i, j int) {
	t[j], t[i] = t[i], t[j]
}
