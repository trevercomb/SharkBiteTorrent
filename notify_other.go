//go:build !windows

package main

import (
	"errors"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

var (
	notifySetup sync.Once
	notifyErr   error
)

// notify uses Wails' notifications: D-Bus on Linux, the Notification Center
// on macOS (which asks the user to allow them the first time).
func (a *App) notify(title, body string) error {
	notifySetup.Do(func() {
		if notifyErr = runtime.InitializeNotifications(a.ctx); notifyErr != nil {
			return
		}
		if ok, err := runtime.RequestNotificationAuthorization(a.ctx); err != nil {
			notifyErr = err
		} else if !ok {
			notifyErr = errors.New("not allowed in the system settings")
		}
	})
	if notifyErr != nil {
		return notifyErr
	}
	return runtime.SendNotification(a.ctx, runtime.NotificationOptions{ID: "finished", Title: title, Body: body})
}
