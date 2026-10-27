// The macOS browser window's Cocoa side (window_darwin.m), for cgo.

#include <stdint.h>

// Implemented in Go (window_darwin.go).
extern void goRunQueue(void);
extern void goLoaded(char *uri);
extern void goPopup(char *uri, int user);
extern int goDownload(char *uri, char *disposition, char *mime, int64_t size);
extern void goResult(int64_t id, char *json);

void sbw_wakeup(void);
int sbw_open_window(const char *proxy, const char *user, const char *pass, const char *store, const char *title);
void sbw_run_app(void);
void sbw_eval_js(const char *js, int64_t qid);
void sbw_load_uri(const char *u);
const char *sbw_page_title(void);
void sbw_set_title(const char *t);
void sbw_present(void);
void sbw_close_window(void);
