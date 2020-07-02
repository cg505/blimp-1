package logs

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/buger/goterm"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kelda/blimp/cli/authstore"
	"github.com/kelda/blimp/cli/manager"
	"github.com/kelda/blimp/pkg/errors"
	"github.com/kelda/blimp/pkg/kubewait"
	"github.com/kelda/blimp/pkg/names"
)

type Command struct {
	Services []string
	Opts     corev1.PodLogOptions
	Auth     authstore.Store
}

type rawLogLine struct {
	// Any error that occurred when trying to read logs.
	// If this is non-nil, `message` and `receivedAt` aren't meaningful.
	error error

	// The container that generated the log.
	fromContainer string

	// The contents of the log line (including the timestamp added by Kubernetes).
	message string

	// The time that we read the log line.
	receivedAt time.Time
}

type parsedLogLine struct {
	// The Kelda container that generated the log.
	fromContainer string

	// The contents of the log line (without the timestamp added by Kubernetes).
	message string

	// The time that the log line was generated by the application according to
	// the machine that the container is running on.
	loggedAt time.Time

	// Specifies the exact string that should be printed for this log line. If
	// this is present, fromContainer and message are both ignored while
	// printing the log.
	formatOverride string
}

func New() *cobra.Command {
	cmd := &Command{}

	cobraCmd := &cobra.Command{
		Use:   "logs SERVICE ...",
		Short: "Print the logs for the given services",
		Long: "Print the logs for the given services.\n\n" +
			"If multiple services are provided, the log output is interleaved.",
		Run: func(_ *cobra.Command, args []string) {
			auth, err := authstore.New()
			if err != nil {
				log.WithError(err).Fatal("Failed to parse auth store")
			}

			if auth.AuthToken == "" {
				fmt.Fprintln(os.Stderr, "Not logged in. Please run `blimp login`.")
				return
			}

			if len(args) == 0 {
				fmt.Fprintf(os.Stderr, "At least one container is required")
				os.Exit(1)
			}

			cmd.Auth = auth
			cmd.Services = args
			if err := cmd.Run(); err != nil {
				errors.HandleFatalError(err)
			}
		},
	}

	cobraCmd.Flags().BoolVarP(&cmd.Opts.Follow, "follow", "f", false,
		"Specify if the logs should be streamed.")
	cobraCmd.Flags().BoolVarP(&cmd.Opts.Previous, "previous", "p", false,
		"If true, print the logs for the previous instance of the container if it crashed.")

	return cobraCmd
}

func (cmd Command) Run() error {
	kubeClient, _, err := cmd.Auth.KubeClient()
	if err != nil {
		return errors.WithContext("connect to cluster", err)
	}

	for _, container := range cmd.Services {
		// For logs to work, the container needs to have started, but it doesn't
		// necessarily need to be running.
		err = manager.CheckServiceStarted(container, cmd.Auth.AuthToken)
		if err != nil {
			return err
		}
	}

	// Exit gracefully when the user Ctrl-C's.
	// The `printLogs` function will return when the context is cancelled,
	// which allows functions defered in this method to run.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-signalChan
		cancel()
	}()

	// The count on the WaitGroup should equal the number of containers we are
	// currently tailing.
	var wg sync.WaitGroup
	combinedLogs := make(chan rawLogLine, len(cmd.Services)*32)
	for _, service := range cmd.Services {
		wg.Add(1)
		go func(service string) {
			for {
				err := cmd.forwardLogs(combinedLogs, service, kubeClient)
				if err != nil {
					// We send the error along the combinedLogs channel so it
					// makes it back to the main thread. `printLogs` can decide
					// how to handle it.
					combinedLogs <- rawLogLine{error: err}
					wg.Done()
					return
				}
				// Indicate that we don't have more logs to send.
				wg.Done()

				// If we aren't following logs, we are done for good.
				if !cmd.Opts.Follow {
					return
				}

				// Wait for a short period of time to see if all the containers
				// exit.
				time.Sleep(500 * time.Millisecond)
				select {
				case <-ctx.Done():
					return
				default:
					// Continue.
				}

				err = waitForRestart(ctx, service, cmd.Auth.KubeNamespace, kubeClient)
				if err != nil {
					// If we get cancelled, don't treat it as an actual error,
					// just return normally.
					if err != context.Canceled {
						log.WithError(err).WithField("service", service).
							Warn("Failed to wait for container to restart")
					}
					return
				}

				// If the container has restarted, start tailing logs again.
				wg.Add(1)
			}
		}(service)
	}

	// If all the containers we were logging have exited, we are done and should
	// exit.
	go func() {
		wg.Wait()
		cancel()
	}()

	hideServiceName := len(cmd.Services) == 1
	return printLogs(ctx, combinedLogs, hideServiceName, cmd.Opts.Follow)
}

// forwardLogs forwards each log line from `logsReq` to the `combinedLogs`
// channel.
func (cmd *Command) forwardLogs(combinedLogs chan<- rawLogLine,
	service string, kubeClient kubernetes.Interface) error {
	// Enable timestamps so that `forwardLogs` can parse the logs.
	cmd.Opts.Timestamps = true
	logsReq := kubeClient.CoreV1().
		Pods(cmd.Auth.KubeNamespace).
		GetLogs(names.PodName(service), &cmd.Opts)

	logsStream, err := logsReq.Stream()
	if err != nil {
		return errors.WithContext("start logs stream", err)
	}
	defer logsStream.Close()
	reader := bufio.NewReader(logsStream)
	for {
		message, err := reader.ReadString('\n')
		combinedLogs <- rawLogLine{
			fromContainer: service,
			message:       strings.TrimSuffix(message, "\n"),
			receivedAt:    time.Now(),
			error:         err,
		}
		if err == io.EOF {
			// Signal to the parent that there will be no more logs for this
			// container, so that the parent can shut down cleanly once all the
			// log streams have ended.
			// We let the consumer of `combinedLogs` decide how to handle all
			// other errors.
			return nil
		}
	}
}

func waitForRestart(ctx context.Context, service, namespace string, kubeClient kubernetes.Interface) error {
	return kubewait.WaitForObject(ctx,
		kubewait.PodGetter(kubeClient, namespace, names.PodName(service)),
		kubeClient.CoreV1().Pods(namespace).Watch,
		func(intf interface{}) bool {
			pod := intf.(*corev1.Pod)
			return pod.Status.Phase == corev1.PodRunning
		},
	)
}

// The logs within a window are guaranteed to be sorted.
// Note that it's still possible for a delayed log to arrive in the next
// window, in which case it will be printed out of order.
const windowSize = 100 * time.Millisecond

// printLogs reads logs from the `rawLogs` in `windowSize` intervals, and
// prints the logs in each window in sorted order.
func printLogs(ctx context.Context, rawLogs <-chan rawLogLine,
	hideServiceName, handleEOF bool) error {
	var window []rawLogLine
	var flushTrigger <-chan time.Time

	// flush prints the logs in the current window to the terminal.
	flush := func() {
		// Parse the logs in the windows to extract their timestamps.
		var parsedLogs []parsedLogLine
		for _, rawLog := range window {
			if rawLog.error == io.EOF {
				// Specially handle EOF.

				// If we got a message (which might be possible), try to parse
				// it.
				if rawLog.message != "" {
					message, timestamp, err := parseLogLine(rawLog.message)
					if err != nil {
						// Don't warn here, this is reasonable.
						message = rawLog.message
						timestamp = rawLog.receivedAt
					}

					parsedLogs = append(parsedLogs, parsedLogLine{
						fromContainer: rawLog.fromContainer,
						message:       message,
						loggedAt:      timestamp,
					})
				}

				// If we are following, let the user know that the container
				// terminated.
				if handleEOF {
					parsedLogs = append(parsedLogs, parsedLogLine{
						loggedAt:       rawLog.receivedAt,
						formatOverride: fmt.Sprintf("The %s container exited.\n", rawLog.fromContainer),
						// We provide reasonable values for these fields even
						// though they should not be used.
						fromContainer: rawLog.fromContainer,
						message:       "container exited",
					})
				}
				continue
			}
			message, timestamp, err := parseLogLine(rawLog.message)

			// If we fail to parse the log's timestamp, revert to sorting based
			// on its receival time.
			if err != nil {
				log.WithField("message", rawLog.message).
					WithField("container", rawLog.fromContainer).
					WithError(err).Warn("Failed to parse timestamp")
				message = rawLog.message
				timestamp = rawLog.receivedAt
			}

			parsedLogs = append(parsedLogs, parsedLogLine{
				fromContainer: rawLog.fromContainer,
				message:       message,
				loggedAt:      timestamp,
			})
		}

		// Sort logs in the window.
		byLogTime := func(i, j int) bool {
			return parsedLogs[i].loggedAt.Before(parsedLogs[j].loggedAt)
		}
		sort.SliceStable(parsedLogs, byLogTime)

		// Print the logs.
		for _, log := range parsedLogs {
			switch {
			case log.formatOverride != "":
				fmt.Fprintf(os.Stdout, "%s", log.formatOverride)

			case hideServiceName:
				fmt.Fprintln(os.Stdout, log.message)

			default:
				coloredContainer := goterm.Color(log.fromContainer, pickColor(log.fromContainer))
				fmt.Fprintf(os.Stdout, "%s › %s\n", coloredContainer, log.message)
			}
		}

		// Clear the buffer now that we've printed its contents.
		window = nil
	}

	defer flush()

	for {
		select {
		case logLine, ok := <-rawLogs:
			if !ok {
				// There won't be any more messages, so we can exit after
				// flushing any unprinted logs.
				return nil
			}

			// If it's an EOF error, still print the final contents of the buffer.
			// We don't need any special handling for ending the stream because
			// the log reader goroutine will just stop sending us messages.
			if logLine.error != nil && logLine.error != io.EOF {
				return errors.WithContext(fmt.Sprintf("read logs for %s", logLine.fromContainer), logLine.error)
			}

			// Wake up later to flush the buffered lines.
			window = append(window, logLine)
			if flushTrigger == nil {
				flushTrigger = time.After(windowSize)
			}
		case <-flushTrigger:
			flush()
			flushTrigger = nil
		case <-ctx.Done():
			// Finish printing any logs that are still on the channel.
			for {
				select {
				case logLine, ok := <-rawLogs:
					if !ok {
						return nil
					}

					// Since we are exiting anyway, ignore logLine.error.
					window = append(window, logLine)
				default:
					return nil
				}
			}
		}
	}
}

func parseLogLine(rawMessage string) (string, time.Time, error) {
	logParts := strings.SplitN(rawMessage, " ", 2)
	if len(logParts) != 2 {
		return "", time.Time{}, errors.New("malformed line")
	}

	rawTimestamp := logParts[0]
	timestamp, err := time.Parse(time.RFC3339Nano, rawTimestamp)
	if err != nil {
		// According to the Kubernetes docs, the timestamp might be in the
		// RFC3339 or RFC3339Nano format.
		timestamp, err = time.Parse(time.RFC3339, rawTimestamp)
		if err != nil {
			return "", time.Time{},
				errors.New("parse timestamp")
		}
	}

	message := logParts[1]
	return message, timestamp, nil
}

var colorList = []int{
	goterm.BLUE,
	goterm.CYAN,
	goterm.GREEN,
	goterm.MAGENTA,
	goterm.RED,
	goterm.YELLOW,
}

func pickColor(container string) int {
	hash := fnv.New32()
	_, err := hash.Write([]byte(container))
	if err != nil {
		panic(err)
	}
	idx := hash.Sum32() % uint32(len(colorList))
	return colorList[idx]
}
