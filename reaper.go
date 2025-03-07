package testcontainers

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/go-connections/nat"

	"github.com/testcontainers/testcontainers-go/internal/testcontainersdocker"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// Deprecated: it has been replaced by the internal testcontainersdocker.LabelLang
	TestcontainerLabel = "org.testcontainers.golang"
	// Deprecated: it has been replaced by the internal testcontainersdocker.LabelSessionID
	TestcontainerLabelSessionID = TestcontainerLabel + ".sessionId"
	// Deprecated: it has been replaced by the internal testcontainersdocker.LabelReaper
	TestcontainerLabelIsReaper = TestcontainerLabel + ".reaper"

	ReaperDefaultImage = "docker.io/testcontainers/ryuk:0.5.1"
)

var (
	reaperInstance *Reaper // We would like to create reaper only once
	reaperMutex    sync.Mutex
	reaperOnce     sync.Once
)

// ReaperProvider represents a provider for the reaper to run itself with
// The ContainerProvider interface should usually satisfy this as well, so it is pluggable
type ReaperProvider interface {
	RunContainer(ctx context.Context, req ContainerRequest) (Container, error)
	Config() TestcontainersConfig
}

// NewReaper creates a Reaper with a sessionID to identify containers and a provider to use
// Deprecated: it's not possible to create a reaper anymore.
func NewReaper(ctx context.Context, sessionID string, provider ReaperProvider, reaperImageName string) (*Reaper, error) {
	return reuseOrCreateReaper(ctx, sessionID, provider, WithImageName(reaperImageName))
}

// reaperContainerNameFromSessionID returns the container name that uniquely
// identifies the container based on the session id.
func reaperContainerNameFromSessionID(sessionID string) string {
	// The session id is 64 characters, so we will not hit the limit of 128
	// characters for container names.
	return fmt.Sprintf("reaper_%s", sessionID)
}

// lookUpReaperContainer returns a DockerContainer type with the reaper container in the case
// it's found in the running state, and including the labels for sessionID, reaper, and ryuk.
// It will perform a retry with exponential backoff to allow for the container to be started and
// avoid potential false negatives.
func lookUpReaperContainer(ctx context.Context, sessionID string) (*DockerContainer, error) {
	dockerClient, err := NewDockerClientWithOpts(ctx)
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	// the backoff will take at most 5 seconds to find the reaper container
	// doing each attempt every 100ms
	exp := backoff.NewExponentialBackOff()

	// we want random intervals between 100ms and 500ms for concurrent executions
	// to not be synchronized: it could be the case that multiple executions of this
	// function happen at the same time (specifically when called from a different test
	// process execution), and we want to avoid that they all try to find the reaper
	// container at the same time.
	exp.InitialInterval = time.Duration(rand.Intn(5)*100) * time.Millisecond
	exp.RandomizationFactor = rand.Float64() * 0.5
	exp.Multiplier = rand.Float64() * 2.0
	exp.MaxInterval = 5.0 * time.Second  // max interval between attempts
	exp.MaxElapsedTime = 1 * time.Minute // max time to keep trying

	var reaperContainer *DockerContainer
	err = backoff.Retry(func() error {
		args := []filters.KeyValuePair{
			filters.Arg("label", fmt.Sprintf("%s=%s", testcontainersdocker.LabelSessionID, sessionID)),
			filters.Arg("label", fmt.Sprintf("%s=%t", testcontainersdocker.LabelReaper, true)),
			filters.Arg("label", fmt.Sprintf("%s=%t", testcontainersdocker.LabelRyuk, true)),
			filters.Arg("name", reaperContainerNameFromSessionID(sessionID)),
		}

		resp, err := dockerClient.ContainerList(ctx, types.ContainerListOptions{
			All:     true,
			Filters: filters.NewArgs(args...),
		})
		if err != nil {
			return err
		}

		if len(resp) == 0 {
			// reaper container not found in the running state: do not look for it again
			return nil
		}

		if len(resp) > 1 {
			return fmt.Errorf("not possible to have multiple reaper containers found for session ID %s", sessionID)
		}

		r, err := containerFromDockerResponse(ctx, resp[0])
		if err != nil {
			return err
		}

		reaperContainer = r

		return nil
	}, backoff.WithContext(exp, ctx))

	if err != nil {
		return nil, err
	}

	return reaperContainer, nil
}

// reuseOrCreateReaper returns an existing Reaper instance if it exists and is running. Otherwise, a new Reaper instance
// will be created with a sessionID to identify containers in the same test session/program.
func reuseOrCreateReaper(ctx context.Context, sessionID string, provider ReaperProvider, opts ...ContainerOption) (*Reaper, error) {
	reaperMutex.Lock()
	defer reaperMutex.Unlock()

	// 1. if the reaper instance has been already created, return it
	if reaperInstance != nil {
		// Verify this instance is still running by checking state.
		// Can't use Container.IsRunning because the bool is not updated when Reaper is terminated
		state, err := reaperInstance.container.State(ctx)
		if err != nil {
			return nil, err
		}

		if state.Running {
			return reaperInstance, nil
		}
		// else: the reaper instance has been terminated, so we need to create a new one
	}

	// 2. because the reaper instance has not been created yet, look for it in the Docker daemon, which
	// will happen if the reaper container has been created in the same test session but in a different
	// test process execution (e.g. when running tests in parallel), not having initialized the reaper
	// instance yet.
	reaperContainer, err := lookUpReaperContainer(context.Background(), sessionID)
	if err == nil && reaperContainer != nil {
		// The reaper container exists as a Docker container: re-use it
		Logger.Printf("🔥 Reaper obtained from Docker for this test session %s", reaperContainer.ID)
		reaperInstance, err = reuseReaperContainer(ctx, sessionID, provider, reaperContainer)
		if err != nil {
			return nil, err
		}
		return reaperInstance, nil
	}

	// 3. the reaper container does not exist in the Docker daemon: create it, and do it using the
	// synchronization primitive to avoid multiple executions of this function to create the reaper
	var reaperErr error
	reaperOnce.Do(func() {
		r, err := newReaper(ctx, sessionID, provider, opts...)
		if err != nil {
			reaperErr = err
			return
		}

		reaperInstance, reaperErr = r, nil
	})
	if reaperErr != nil {
		reaperOnce = sync.Once{}
		return nil, reaperErr
	}

	return reaperInstance, nil
}

var createContainerFailDueToNameConflictRegex = regexp.MustCompile("Conflict. The container name .* is already in use by container .*")

// reuseReaperContainer constructs a Reaper from an already running reaper
// DockerContainer.
func reuseReaperContainer(ctx context.Context, sessionID string, provider ReaperProvider, reaperContainer *DockerContainer) (*Reaper, error) {
	endpoint, err := reaperContainer.PortEndpoint(ctx, "8080", "")
	if err != nil {
		return nil, err
	}
	return &Reaper{
		Provider:  provider,
		SessionID: sessionID,
		Endpoint:  endpoint,
		container: reaperContainer,
	}, nil
}

// newReaper creates a Reaper with a sessionID to identify containers and a
// provider to use. Do not call this directly, use reuseOrCreateReaper instead.
func newReaper(ctx context.Context, sessionID string, provider ReaperProvider, opts ...ContainerOption) (*Reaper, error) {
	dockerHostMount := testcontainersdocker.ExtractDockerSocket(ctx)

	reaper := &Reaper{
		Provider:  provider,
		SessionID: sessionID,
	}

	listeningPort := nat.Port("8080/tcp")

	tcConfig := provider.Config().Config

	reaperOpts := containerOptions{}

	for _, opt := range opts {
		opt(&reaperOpts)
	}

	req := ContainerRequest{
		Image:         reaperImage(reaperOpts.ImageName),
		ExposedPorts:  []string{string(listeningPort)},
		Labels:        testcontainersdocker.DefaultLabels(sessionID),
		Privileged:    tcConfig.RyukPrivileged,
		WaitingFor:    wait.ForListeningPort(listeningPort),
		Name:          reaperContainerNameFromSessionID(sessionID),
		ReaperOptions: opts,
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.AutoRemove = true
			hc.Binds = []string{dockerHostMount + ":/var/run/docker.sock"}
			hc.NetworkMode = Bridge
		},
		Env: map[string]string{},
	}
	if to := tcConfig.RyukConnectionTimeout; to > time.Duration(0) {
		req.Env["RYUK_CONNECTION_TIMEOUT"] = to.String()
	}
	if to := tcConfig.RyukReconnectionTimeout; to > time.Duration(0) {
		req.Env["RYUK_RECONNECTION_TIMEOUT"] = to.String()
	}

	// keep backwards compatibility
	req.ReaperImage = req.Image

	// include reaper-specific labels to the reaper container
	req.Labels[testcontainersdocker.LabelReaper] = "true"
	req.Labels[testcontainersdocker.LabelRyuk] = "true"

	// Attach reaper container to a requested network if it is specified
	if p, ok := provider.(*DockerProvider); ok {
		req.Networks = append(req.Networks, p.DefaultNetwork)
	}

	c, err := provider.RunContainer(ctx, req)
	if err != nil {
		// We need to check whether the error is caused by a container with the same name
		// already existing due to race conditions. We manually match the error message
		// as we do not have any error types to check against.
		if createContainerFailDueToNameConflictRegex.MatchString(err.Error()) {
			// Manually retrieve the already running reaper container. However, we need to
			// use retries here as there are two possible race conditions that might lead to
			// errors: In most cases, there is a small delay between container creation and
			// actually being visible in list-requests. This means that creation might fail
			// due to name conflicts, but when we list containers with this name, we do not
			// get any results. In another case, the container might have simply died in the
			// meantime and therefore cannot be found.
			const timeout = 5 * time.Second
			const cooldown = 100 * time.Millisecond
			start := time.Now()
			var reaperContainer *DockerContainer
			for time.Since(start) < timeout {
				reaperContainer, err = lookUpReaperContainer(ctx, sessionID)
				if err == nil && reaperContainer != nil {
					break
				}
				select {
				case <-ctx.Done():
				case <-time.After(cooldown):
				}
			}
			if err != nil {
				return nil, fmt.Errorf("look up reaper container due to name conflict failed: %w", err)
			}
			// If the reaper container was not found, it is most likely to have died in
			// between as we can exclude any client errors because of the previous error
			// check. Because the reaper should only die if it performed clean-ups, we can
			// fail here as the reaper timeout needs to be increased, anyway.
			if reaperContainer == nil {
				return nil, fmt.Errorf("look up reaper container returned nil although creation failed due to name conflict")
			}
			Logger.Printf("🔥 Reaper obtained from Docker for this test session %s", reaperContainer.ID)
			reaper, err := reuseReaperContainer(ctx, sessionID, provider, reaperContainer)
			if err != nil {
				return nil, err
			}
			return reaper, nil
		}
		return nil, err
	}
	reaper.container = c

	endpoint, err := c.PortEndpoint(ctx, "8080", "")
	if err != nil {
		return nil, err
	}
	reaper.Endpoint = endpoint

	return reaper, nil
}

// Reaper is used to start a sidecar container that cleans up resources
type Reaper struct {
	Provider  ReaperProvider
	SessionID string
	Endpoint  string
	container Container
}

// Connect runs a goroutine which can be terminated by sending true into the returned channel
func (r *Reaper) Connect() (chan bool, error) {
	conn, err := net.DialTimeout("tcp", r.Endpoint, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w: Connecting to Ryuk on %s failed", err, r.Endpoint)
	}

	terminationSignal := make(chan bool)
	go func(conn net.Conn) {
		sock := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		defer conn.Close()

		labelFilters := []string{}
		for l, v := range testcontainersdocker.DefaultLabels(r.SessionID) {
			labelFilters = append(labelFilters, fmt.Sprintf("label=%s=%s", l, v))
		}

		retryLimit := 3
		for retryLimit > 0 {
			retryLimit--

			if _, err := sock.WriteString(strings.Join(labelFilters, "&")); err != nil {
				continue
			}

			if _, err := sock.WriteString("\n"); err != nil {
				continue
			}

			if err := sock.Flush(); err != nil {
				continue
			}

			resp, err := sock.ReadString('\n')
			if err != nil {
				continue
			}

			if resp == "ACK\n" {
				break
			}
		}

		<-terminationSignal
	}(conn)
	return terminationSignal, nil
}

// Labels returns the container labels to use so that this Reaper cleans them up
// Deprecated: internally replaced by testcontainersdocker.DefaultLabels(sessionID)
func (r *Reaper) Labels() map[string]string {
	return map[string]string{
		testcontainersdocker.LabelLang:      "go",
		testcontainersdocker.LabelSessionID: r.SessionID,
	}
}

func reaperImage(reaperImageName string) string {
	if reaperImageName == "" {
		return ReaperDefaultImage
	}
	return reaperImageName
}
