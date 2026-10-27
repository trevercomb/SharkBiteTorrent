package browser

// Shared by the Windows (WebView2), Linux (WebKitGTK) and macOS (WKWebView)
// windows.

// Options for a browser window.
type Options struct {
	// Proxy is the 127.0.0.1:port of a SOCKS5 proxy without a password
	// (Chromium can't log in to one) that sends everything through the VPN.
	Proxy string
	// ProxyUser and ProxyPass log in to it, where the window can (macOS;
	// see GuardProxy).
	ProxyUser, ProxyPass string
	// DataDir holds the window's own browser profile (cookies, cache).
	DataDir string
	Title   string
	// Args are extra Chromium switches (e.g. features to turn off).
	Args []string

	// Called on the window's thread.
	OnDownload func(Download) DownloadChoice
	OnLoaded   func(url string)
	OnPopup    func(NewWindow)
	OnClosed   func() // the window is gone (closed by the user or Close)
}

// NewWindow describes a pop-up the page tried to open.
type NewWindow struct {
	URL         string
	UserStarted bool // from a click, rather than a script on its own
}

// Download describes a download the page started.
type Download struct {
	URL         string
	Disposition string // Content-Disposition, which usually holds the file name
	MimeType    string
	Size        int64 // -1 if unknown
}

// DownloadChoice is what to do with a download that's starting.
type DownloadChoice struct {
	Cancel bool   // the app downloads it itself
	Path   string // otherwise, where the browser saves it ("" = its default)
}
