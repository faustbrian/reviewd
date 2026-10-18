package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/faustbrian/reviewd/internal/config"
	"github.com/faustbrian/reviewd/internal/dispatch"
	"github.com/faustbrian/reviewd/internal/github"
	"github.com/faustbrian/reviewd/internal/notify"
	"github.com/faustbrian/reviewd/internal/store"
	"github.com/faustbrian/reviewd/internal/terminal"
	"github.com/faustbrian/reviewd/internal/watch"
	"github.com/spf13/cobra"
)

func (e *execution) watchCommand(deps Dependencies) *cobra.Command {
	var headless, once bool
	command := &cobra.Command{Use: "watch", Short: "Watch the review inbox and process eligible decisions", Args: cobra.NoArgs}
	command.Flags().BoolVar(&headless, "headless", false, "Run without a terminal attachment")
	command.Flags().BoolVar(&once, "once", false, "Synchronize and drain currently eligible work, then exit")
	command.RunE = func(command *cobra.Command, _ []string) (resultErr error) {
		if len(e.overrides(command)) != 0 {
			return failure(2, "select model overrides on an individual review or start decision")
		}
		if !headless && !once && !e.json && terminal.IsTerminal(command.InOrStdin(), command.OutOrStdout()) {
			return e.attachInbox(command, deps, true)
		}
		_, path, err := e.load(command, false)
		if err != nil {
			return failure(2, err.Error())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return failure(2, "cannot read watcher configuration")
		}
		manager, err := config.NewManager(data, path)
		if err != nil {
			return failure(2, err.Error())
		}
		policy, err := config.ResolveDefaults(manager.Document(), nil)
		if err != nil {
			return failure(2, err.Error())
		}
		directory, err := stateDirectory(policy, path)
		if err != nil {
			return failure(1, err.Error())
		}
		durable, err := store.Open(command.Context(), directory)
		if err != nil {
			return failure(1, err.Error())
		}
		defer durable.Close()
		lease, err := watch.LockProfile(directory)
		if err != nil {
			return failure(1, err.Error())
		}
		defer lease.Close()
		e.recoverBeforeWork(command, durable)
		lastRecovery := time.Now()
		pumpRecovery := func() {
			if time.Since(lastRecovery) < time.Minute {
				return
			}
			lastRecovery = time.Now()
			e.recoverBeforeWork(command, durable)
		}
		if err := durable.RecoverScheduled(command.Context()); err != nil {
			return failure(1, err.Error())
		}
		lastRetention := time.Time{}
		pumpRetention := func() {
			now := time.Now()
			if !lastRetention.IsZero() && now.Sub(lastRetention) < time.Hour {
				return
			}
			lastRetention = now
			current, err := config.ResolveDefaults(manager.Document(), nil)
			if err != nil {
				return
			}
			if _, err := dispatch.ExpireHistory(command.Context(), durable, int(current.Int("state.retention_days")), false, now); err != nil && command.Context().Err() == nil {
				fmt.Fprintln(command.ErrOrStderr(), "Completed agent history could not expire:", terminalText(err.Error()))
			}
		}
		desktop := deps.Desktop
		if desktop == nil {
			desktop = notify.Desktop()
		}
		notificationWarning := false
		notifications := notify.Service{Store: durable, Desktop: desktop, Terminal: notify.Terminal(command.ErrOrStderr()), Warn: func(error) {
			if !notificationWarning {
				fmt.Fprintln(command.ErrOrStderr(), "Notification delivery unavailable; all decisions remain in reviewd inbox.")
				notificationWarning = true
			}
		}}
		lastNotification := time.Time{}
		pumpNotifications := func(force bool) error {
			if !force && time.Since(lastNotification) < time.Second {
				return nil
			}
			lastNotification = time.Now()
			return notifications.Pump(command.Context(), manager.Document(), force)
		}
		engine := watch.Engine{Store: durable, Configuration: manager, Connect: deps.GitHub, Execute: e.itemExecutor(deps, durable, directory)}
		defer func() { resultErr = errors.Join(resultErr, engine.Close()) }()
		var lastReloadError string
		failures := 0
		for {
			if next, readErr := os.ReadFile(path); readErr != nil {
				if lastReloadError != "Cannot reload configuration; keeping the last valid settings." {
					fmt.Fprintln(command.ErrOrStderr(), "Cannot reload configuration; keeping the last valid settings.")
				}
				lastReloadError = "Cannot reload configuration; keeping the last valid settings."
			} else {
				candidate, parseErr := config.Parse(next, path)
				if parseErr == nil {
					resolved, resolveErr := config.ResolveDefaults(candidate, nil)
					if resolveErr != nil {
						parseErr = resolveErr
					} else if nextDirectory, dirErr := stateDirectory(resolved, path); dirErr != nil {
						parseErr = dirErr
					} else if nextDirectory != directory {
						parseErr = errors.New("state directory changed; restart in the intended profile instead of moving active work")
					}
				}
				if parseErr == nil {
					parseErr = manager.Reload(next)
				}
				if parseErr != nil {
					message := "Configuration reload rejected; keeping the last valid settings: " + redact(parseErr.Error())
					if message != lastReloadError {
						fmt.Fprintln(command.ErrOrStderr(), terminalText(message))
					}
					lastReloadError = message
				} else {
					lastReloadError = ""
				}
			}
			pumpRetention()
			pumpRecovery()
			err := engine.Tick(command.Context())
			if err != nil {
				failures++
				fmt.Fprintln(command.ErrOrStderr(), terminalText(redact(err.Error())))
			} else {
				failures = 0
			}
			if once {
				for engine.Active() > 0 {
					err = errors.Join(err, engine.Complete(command.Context(), true))
					if command.Context().Err() != nil {
						return failure(1, "watch interrupted; unfinished agent work remains inspectable")
					}
					if refillErr := engine.Admit(command.Context()); refillErr != nil {
						err = errors.Join(err, refillErr)
						break
					}
				}
				if notificationErr := pumpNotifications(true); notificationErr != nil {
					return failure(1, notificationErr.Error())
				}
				if presentErr := e.presentInbox(command.Context(), command.OutOrStdout(), durable); presentErr != nil {
					return presentErr
				}
				if err != nil {
					return failure(1, err.Error())
				}
				return nil
			}
			if notificationErr := pumpNotifications(false); notificationErr != nil {
				return failure(1, notificationErr.Error())
			}
			if presentErr := e.presentInbox(command.Context(), command.OutOrStdout(), durable); presentErr != nil {
				return presentErr
			}
			// Remote acquisition failures hold future admission and use the existing
			// backoff; they must not shut down accepted skills with the watcher.
			current, resolveErr := config.ResolveDefaults(manager.Document(), nil)
			if resolveErr != nil {
				return failure(2, resolveErr.Error())
			}
			timer := time.NewTimer(watchDelay(current.Duration("watch.poll_interval"), failures, err))
			waiting := true
			for waiting {
				select {
				case <-command.Context().Done():
					timer.Stop()
					return nil
				case <-timer.C:
					waiting = false
				default:
					pumpRecovery()
					if finishErr := engine.Complete(command.Context(), false); finishErr != nil {
						fmt.Fprintln(command.ErrOrStderr(), terminalText(redact(finishErr.Error())))
					}
					if notificationErr := pumpNotifications(false); notificationErr != nil {
						timer.Stop()
						return failure(1, notificationErr.Error())
					}
					select {
					case <-command.Context().Done():
						timer.Stop()
						return nil
					case <-timer.C:
						waiting = false
					case <-time.After(50 * time.Millisecond):
					}
				}
			}
		}
	}
	return command
}

func watchDelay(base time.Duration, failures int, err error) time.Duration {
	delay := base
	for attempt := 1; attempt < failures; attempt++ {
		if delay >= 15*time.Minute {
			break
		}
		if delay > 15*time.Minute/2 {
			delay = 15 * time.Minute
		} else {
			delay *= 2
		}
	}
	if retry := github.RetryAfter(err); retry > delay {
		delay = retry
	}
	return delay
}
