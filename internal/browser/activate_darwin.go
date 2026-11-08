//go:build darwin && cgo

package browser

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

// Since macOS 14 an app comes to the front only when the active one lets
// it: the app (in front, as the user just used it) lets the window's
// process, which then activates itself.
static void sbw_yield_to(int pid) {
	if (@available(macOS 14.0, *)) {
		void (^yield)(void) = ^{
			NSRunningApplication *app = [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
			if (app && NSApp) [NSApp yieldActivationToApplication:app];
		};
		// AppKit, so on the main thread, and before the window is told to
		// come forward; never waiting long (no app, or its thread is busy).
		if (!NSApp) return;
		if ([NSThread isMainThread]) {
			yield();
			return;
		}
		dispatch_semaphore_t done = dispatch_semaphore_create(0);
		dispatch_async(dispatch_get_main_queue(), ^{
			yield();
			dispatch_semaphore_signal(done);
		});
		dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC));
	}
}
*/
import "C"

// letForward lets the window's process (pid) come to the front.
func letForward(pid int) { C.sbw_yield_to(C.int(pid)) }
