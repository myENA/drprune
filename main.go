package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cego/docker-registry-pruner/registry"
	"github.com/hashicorp/consul/api"
	nomad "github.com/hashicorp/nomad/api"
	"github.com/myENA/consul-decoder"
	"github.com/myENA/drprune/models"
	"github.com/rs/zerolog"
)

const drROKey = "REGISTRY_STORAGE_MAINTENANCE_READONLY"

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
	var nomadJob string
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
		&nomadJob,
		"nomad-job",
		"docker-registry",
		"this is the nomad job for the docker-registry - used for GC",
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
				if tag == "latest" {
					continue
				}
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
				sort.Sort(sort.Reverse(releaseTags))
				for _, rt := range releaseTags[c.MinReleaseImages:] {
					d := time.Since(rt.created)
					if d > time.Duration(int(time.Hour)*24*c.MinReleaseEvictionDays) {
						tagsToDelete = append(tagsToDelete, rt)
					}
				}
			}

		}

		sort.Sort(tagsToDelete)
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
	}
}

func runGC(nomadJob string) error {
	c, err := nomad.NewClient(nomad.DefaultConfig())
	if err != nil {
		return fmt.Errorf("could not create nomad client: %w", err)
	}

	job, _, err := c.Jobs().Info(nomadJob, nil)
	if err != nil {
		return fmt.Errorf("could not get information for %s: %w", nomadJob, err)
	}

	_, _, err = c.Jobs().Deregister(nomadJob, true, nil)
	if err != nil {
		return fmt.Errorf("could not shut down nomad job: %w", err)
	}

	JobSetReadOnlyEnv(job)

	jerb, _, err := c.Jobs().Register(job, nil)
	if err != nil {
		return fmt.Errorf("could not register gc job: %w", err)
	}

	var allocID string
	var lastIndex uint64
	for i := 0; i < 10; i++ {
		evals, meta, err := c.Evaluations().Allocations(jerb.EvalID, &nomad.QueryOptions{
			WaitIndex: lastIndex,
			WaitTime:  time.Second * 60,
		})

		if err != nil {
			return fmt.Errorf("could not get evaluations: %w", err)
		}

		lastIndex = meta.LastIndex
		if len(evals) > 0 {
			ev := evals[0]
			if ev.ClientStatus == nomad.AllocClientStatusRunning {
				allocID = ev.ID
				break
			}
			log.Info().Msg("status not yet healthy, retrying")
		} else {
			log.Info().Msg("no evaluations returned, retrying")
		}
	}
	if len(allocID) == 0 {
		return fmt.Errorf("could not get allocation")
	}

	lastIndex = 0
	var alloc *nomad.Allocation
	for i := 0; i < 10; i++ {
		var meta *nomad.QueryMeta
		var err error
		alloc, meta, err = c.Allocations().Info(allocID, &nomad.QueryOptions{
			WaitIndex: lastIndex,
			WaitTime:  time.Second * 60,
		})
		if err != nil {
			return fmt.Errorf("fatal error fetching allocs for %w", err)
		}
		lastIndex = meta.LastIndex
		if alloc != nil && alloc.ClientStatus == nomad.AllocClientStatusRunning {
			break
		}

		log.Info().Msg("alloc not found, retrying")

	}

	buf := &bytes.Buffer{}

	s := bufio.NewScanner(buf)

	name := job.TaskGroups[0].Tasks[0].Name
	log.Debug().Msgf("task name: %s, allocID: %s", name, alloc.ID)
	ex, err := c.Allocations().Exec(context.Background(), alloc, job.TaskGroups[0].Tasks[0].Name, false,
		[]string{"ps", "-o", "pid,comm,args"}, nil, buf, nil, nil, nil)

	if ex != 0 {
		return fmt.Errorf("non-zero exit code: %d", ex)
	}
	if err != nil {
		return fmt.Errorf("error from exec: %w", err)
	}

	psRecs, err := parsePS(s)
	if err != nil {
		return fmt.Errorf("could not parse ps response: %w", err)
	}

	var (
		cfgFile, cmd string
	)

	for _, psr := range psRecs {
		if strings.HasSuffix(psr.cmd, "registry") {
			for _, arg := range psr.args {
				if strings.HasSuffix(arg, ".yml") {
					cfgFile = arg
					cmd = psr.cmd
					break
				}
			}
		}
	}

	if len(cfgFile) == 0 {
		return fmt.Errorf("could not deteremine config file to use for GC")
	}
	if len(cmd) == 0 {
		return fmt.Errorf("could not determine command that was run")
	}

	ex, err = c.Allocations().Exec(context.Background(), alloc, job.TaskGroups[0].Tasks[0].Name, false,
		[]string{cmd, "garbage-collect", cfgFile}, nil, os.Stdout, os.Stderr, nil, nil)

	if err != nil {
		return fmt.Errorf("error running GC: %w", err)
	}
	if ex != 0 {
		return fmt.Errorf("non-zero exit status: %d", ex)
	}
	return nil
}

type psRec struct {
	pid  int
	cmd  string
	args []string
}

func parsePS(s *bufio.Scanner) ([]psRec, error) {
	first := true
	rex := regexp.MustCompile(`^\s*([0-9]+)\s*([^\s]+)\s*(.*)?\s*$`)
	var psrs []psRec
	for s.Scan() {
		if first {
			first = false
			continue
		}
		line := s.Text()
		matches := rex.FindStringSubmatch(line)
		if len(matches) != 4 {
			return nil, fmt.Errorf("invalid line %s", line)
		}
		var (
			psr psRec
			err error
		)
		psr.pid, err = strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("error converting pid to number: %w", err)
		}
		psr.cmd = matches[2]
		psr.args = strings.Split(matches[3], " ")
		psrs = append(psrs, psr)
	}
	return psrs, nil
}

func JobSetReadOnlyEnv(job *nomad.Job) error {
	if len(job.TaskGroups) == 0 ||
		len(job.TaskGroups[0].Tasks) == 0 {
		return fmt.Errorf("invalid job - cannot find task")
	}

	t := job.TaskGroups[0].Tasks[0]

	t.Env[drROKey] = `{"enabled":true}`
	return nil
}

func JobClearReadOnlyEnv(job *nomad.Job) error {
	if len(job.TaskGroups) == 0 ||
		len(job.TaskGroups[0].Tasks) == 0 {
		return fmt.Errorf("invalid job - cannot find task")
	}

	t := job.TaskGroups[0].Tasks[0]

	delete(t.Env, drROKey)
	return nil
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
