export namespace engine {
	
	export class FileStatus {
	    index: number;
	    path: string;
	    size: number;
	    done: number;
	    wanted: boolean;
	
	    static createFrom(source: any = {}) {
	        return new FileStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.path = source["path"];
	        this.size = source["size"];
	        this.done = source["done"];
	        this.wanted = source["wanted"];
	    }
	}
	export class PieceRun {
	    s: string;
	    n: number;
	
	    static createFrom(source: any = {}) {
	        return new PieceRun(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.s = source["s"];
	        this.n = source["n"];
	    }
	}
	export class SFVStatus {
	    checked: number;
	    bad: string[];
	    missing: string[];
	    error: string;
	    checking: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SFVStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.checked = source["checked"];
	        this.bad = source["bad"];
	        this.missing = source["missing"];
	        this.error = source["error"];
	        this.checking = source["checking"];
	    }
	}
	export class VerifyResult {
	    pieces: number;
	    bad: number;
	    badFiles: string[];
	    at: number;
	    reason: string;
	    unreadable: number;
	
	    static createFrom(source: any = {}) {
	        return new VerifyResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.pieces = source["pieces"];
	        this.bad = source["bad"];
	        this.badFiles = source["badFiles"];
	        this.at = source["at"];
	        this.reason = source["reason"];
	        this.unreadable = source["unreadable"];
	    }
	}
	export class Status {
	    hash: string;
	    name: string;
	    dir: string;
	    state: string;
	    size: number;
	    done: number;
	    progress: number;
	    downRate: number;
	    upRate: number;
	    peers: number;
	    seeds: number;
	    warnings: scan.Warning[];
	    problem: string;
	    held: boolean;
	    confirm: boolean;
	    scanState: string;
	    scanEngine: string;
	    threats: string[];
	    scanError: string;
	    tooLarge: string[];
	    canScanFully: boolean;
	    scanSeen: boolean;
	    verifying: boolean;
	    verifyProgress: number;
	    verified?: VerifyResult;
	    sfv?: SFVStatus;
	    suspect: string;
	
	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.hash = source["hash"];
	        this.name = source["name"];
	        this.dir = source["dir"];
	        this.state = source["state"];
	        this.size = source["size"];
	        this.done = source["done"];
	        this.progress = source["progress"];
	        this.downRate = source["downRate"];
	        this.upRate = source["upRate"];
	        this.peers = source["peers"];
	        this.seeds = source["seeds"];
	        this.warnings = this.convertValues(source["warnings"], scan.Warning);
	        this.problem = source["problem"];
	        this.held = source["held"];
	        this.confirm = source["confirm"];
	        this.scanState = source["scanState"];
	        this.scanEngine = source["scanEngine"];
	        this.threats = source["threats"];
	        this.scanError = source["scanError"];
	        this.tooLarge = source["tooLarge"];
	        this.canScanFully = source["canScanFully"];
	        this.scanSeen = source["scanSeen"];
	        this.verifying = source["verifying"];
	        this.verifyProgress = source["verifyProgress"];
	        this.verified = this.convertValues(source["verified"], VerifyResult);
	        this.sfv = this.convertValues(source["sfv"], SFVStatus);
	        this.suspect = source["suspect"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace main {
	
	export class ProviderLink {
	    url: string;
	    referral: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ProviderLink(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.referral = source["referral"];
	    }
	}
	export class Links {
	    repo: string;
	    sponsor: string;
	    providers: Record<string, ProviderLink>;
	
	    static createFrom(source: any = {}) {
	        return new Links(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.repo = source["repo"];
	        this.sponsor = source["sponsor"];
	        this.providers = this.convertValues(source["providers"], ProviderLink, true);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class About {
	    version: string;
	    license: string;
	    links: Links;
	    funding: boolean;
	    store: boolean;
	
	    static createFrom(source: any = {}) {
	        return new About(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.license = source["license"];
	        this.links = this.convertValues(source["links"], Links);
	        this.funding = source["funding"];
	        this.store = source["store"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class AddLinksResult {
	    torrents: number;
	    files: number;
	    pages: number;
	    errors: string[];
	
	    static createFrom(source: any = {}) {
	        return new AddLinksResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.torrents = source["torrents"];
	        this.files = source["files"];
	        this.pages = source["pages"];
	        this.errors = source["errors"];
	    }
	}
	export class DownloadStatus {
	    id: string;
	    name: string;
	    dir: string;
	    page: string;
	    size: number;
	    have: number;
	    rate: number;
	    state: string;
	    error: string;
	    offline: boolean;
	    scan: string;
	    scanEngine: string;
	    threats: string[];
	    scanError: string;
	
	    static createFrom(source: any = {}) {
	        return new DownloadStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.dir = source["dir"];
	        this.page = source["page"];
	        this.size = source["size"];
	        this.have = source["have"];
	        this.rate = source["rate"];
	        this.state = source["state"];
	        this.error = source["error"];
	        this.offline = source["offline"];
	        this.scan = source["scan"];
	        this.scanEngine = source["scanEngine"];
	        this.threats = source["threats"];
	        this.scanError = source["scanError"];
	    }
	}
	export class DownloadedUpdate {
	    path: string;
	    hint: string;
	    command: string;
	
	    static createFrom(source: any = {}) {
	        return new DownloadedUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.hint = source["hint"];
	        this.command = source["command"];
	    }
	}
	
	export class LoginResult {
	    display: string;
	    needField?: vpnprov.Field;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new LoginResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.display = source["display"];
	        this.needField = this.convertValues(source["needField"], vpnprov.Field);
	        this.message = source["message"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class NoteGroup {
	    title: string;
	    items: string[];
	
	    static createFrom(source: any = {}) {
	        return new NoteGroup(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.title = source["title"];
	        this.items = source["items"];
	    }
	}
	
	export class ProxyCheck {
	    used: boolean;
	    connections: number;
	
	    static createFrom(source: any = {}) {
	        return new ProxyCheck(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.used = source["used"];
	        this.connections = source["connections"];
	    }
	}
	export class ProxyStatus {
	    enabled: boolean;
	    running: boolean;
	    applied: boolean;
	    manual: boolean;
	    host: string;
	    port: number;
	    username: string;
	    password: string;
	    error: string;
	
	    static createFrom(source: any = {}) {
	        return new ProxyStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.running = source["running"];
	        this.applied = source["applied"];
	        this.manual = source["manual"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.username = source["username"];
	        this.password = source["password"];
	        this.error = source["error"];
	    }
	}
	export class ReleaseNote {
	    version: string;
	    date: string;
	    text: string[];
	    groups: NoteGroup[];
	
	    static createFrom(source: any = {}) {
	        return new ReleaseNote(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.date = source["date"];
	        this.text = source["text"];
	        this.groups = this.convertValues(source["groups"], NoteGroup);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RemoteDeviceView {
	    id: string;
	    name: string;
	    ip: string;
	    viaTailscale: boolean;
	    tsName: string;
	    created: number;
	    lastSeen: number;
	
	    static createFrom(source: any = {}) {
	        return new RemoteDeviceView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.ip = source["ip"];
	        this.viaTailscale = source["viaTailscale"];
	        this.tsName = source["tsName"];
	        this.created = source["created"];
	        this.lastSeen = source["lastSeen"];
	    }
	}
	export class RemotePairing {
	    url: string;
	    code: string;
	    qr: string;
	    expires: number;
	
	    static createFrom(source: any = {}) {
	        return new RemotePairing(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.code = source["code"];
	        this.qr = source["qr"];
	        this.expires = source["expires"];
	    }
	}
	export class RemoteStatus {
	    enabled: boolean;
	    running: boolean;
	    paused: string;
	    tailscale: boolean;
	    lanAvailable: boolean;
	    tailscaleURL: string;
	    tailscaleFound: boolean;
	    home: string;
	    port: number;
	    urls: string[];
	    error: string;
	    devices: RemoteDeviceView[];
	
	    static createFrom(source: any = {}) {
	        return new RemoteStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.running = source["running"];
	        this.paused = source["paused"];
	        this.tailscale = source["tailscale"];
	        this.lanAvailable = source["lanAvailable"];
	        this.tailscaleURL = source["tailscaleURL"];
	        this.tailscaleFound = source["tailscaleFound"];
	        this.home = source["home"];
	        this.port = source["port"];
	        this.urls = source["urls"];
	        this.error = source["error"];
	        this.devices = this.convertValues(source["devices"], RemoteDeviceView);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SearchStatus {
	    configured: boolean;
	    kind: string;
	    url: string;
	
	    static createFrom(source: any = {}) {
	        return new SearchStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.configured = source["configured"];
	        this.kind = source["kind"];
	        this.url = source["url"];
	    }
	}
	export class Settings {
	    downloadDir: string;
	    vpnProvider: string;
	    forwardedPort: number;
	    accountProvider: string;
	    accountRegion: string;
	    downLimitKB: number;
	    upLimitKB: number;
	    closeToTray: boolean;
	    autoUpdateCheck: boolean;
	    skippedVersion: string;
	    lastUpdateCheck: number;
	    updateChannel: string;
	    scanDownloads: boolean;
	    autoVerify: string;
	    remoteEnabled: boolean;
	    remotePort: number;
	    remoteTailscale: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.downloadDir = source["downloadDir"];
	        this.vpnProvider = source["vpnProvider"];
	        this.forwardedPort = source["forwardedPort"];
	        this.accountProvider = source["accountProvider"];
	        this.accountRegion = source["accountRegion"];
	        this.downLimitKB = source["downLimitKB"];
	        this.upLimitKB = source["upLimitKB"];
	        this.closeToTray = source["closeToTray"];
	        this.autoUpdateCheck = source["autoUpdateCheck"];
	        this.skippedVersion = source["skippedVersion"];
	        this.lastUpdateCheck = source["lastUpdateCheck"];
	        this.updateChannel = source["updateChannel"];
	        this.scanDownloads = source["scanDownloads"];
	        this.autoVerify = source["autoVerify"];
	        this.remoteEnabled = source["remoteEnabled"];
	        this.remotePort = source["remotePort"];
	        this.remoteTailscale = source["remoteTailscale"];
	    }
	}
	export class SettingsView {
	    settings: Settings;
	    customEndpoint: string;
	    assoc: platform.AssocStatus;
	    accounts: Record<string, string>;
	    scanner: string;
	    scannerAvailable: boolean;
	    keyringMissing: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SettingsView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.settings = this.convertValues(source["settings"], Settings);
	        this.customEndpoint = source["customEndpoint"];
	        this.assoc = this.convertValues(source["assoc"], platform.AssocStatus);
	        this.accounts = source["accounts"];
	        this.scanner = source["scanner"];
	        this.scannerAvailable = source["scannerAvailable"];
	        this.keyringMissing = source["keyringMissing"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class StartupStatus {
	    enabled: boolean;
	    viaWindowsSettings: boolean;
	
	    static createFrom(source: any = {}) {
	        return new StartupStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.viaWindowsSettings = source["viaWindowsSettings"];
	    }
	}
	export class UpdateInfo {
	    current: string;
	    latest: string;
	    available: boolean;
	    notesURL: string;
	    canInstall: boolean;
	    canDownload: boolean;
	    direct: boolean;
	    notes: ReleaseNote[];
	
	    static createFrom(source: any = {}) {
	        return new UpdateInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.current = source["current"];
	        this.latest = source["latest"];
	        this.available = source["available"];
	        this.notesURL = source["notesURL"];
	        this.canInstall = source["canInstall"];
	        this.canDownload = source["canDownload"];
	        this.direct = source["direct"];
	        this.notes = this.convertValues(source["notes"], ReleaseNote);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class VpnStatus {
	    state: string;
	    provider: string;
	    name: string;
	    server: string;
	    exitIP: string;
	    location: string;
	    colo: string;
	    port: number;
	    warning: string;
	    error: string;
	
	    static createFrom(source: any = {}) {
	        return new VpnStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.provider = source["provider"];
	        this.name = source["name"];
	        this.server = source["server"];
	        this.exitIP = source["exitIP"];
	        this.location = source["location"];
	        this.colo = source["colo"];
	        this.port = source["port"];
	        this.warning = source["warning"];
	        this.error = source["error"];
	    }
	}

}

export namespace paste {
	
	export class Link {
	    url: string;
	    host: string;
	    name: string;
	    kind: string;
	
	    static createFrom(source: any = {}) {
	        return new Link(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.host = source["host"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	    }
	}

}

export namespace platform {
	
	export class AssocStatus {
	    supported: boolean;
	    magnet: boolean;
	    torrent: boolean;
	    canOpenSettings: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AssocStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.supported = source["supported"];
	        this.magnet = source["magnet"];
	        this.torrent = source["torrent"];
	        this.canOpenSettings = source["canOpenSettings"];
	    }
	}

}

export namespace scan {
	
	export class Warning {
	    file: string;
	    reason: string;
	
	    static createFrom(source: any = {}) {
	        return new Warning(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.file = source["file"];
	        this.reason = source["reason"];
	    }
	}

}

export namespace search {
	
	export class Result {
	    title: string;
	    size: number;
	    seeders: number;
	    leechers: number;
	    indexer: string;
	    published: string;
	    magnet?: string;
	    download?: string;
	    infoUrl?: string;
	
	    static createFrom(source: any = {}) {
	        return new Result(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.title = source["title"];
	        this.size = source["size"];
	        this.seeders = source["seeders"];
	        this.leechers = source["leechers"];
	        this.indexer = source["indexer"];
	        this.published = source["published"];
	        this.magnet = source["magnet"];
	        this.download = source["download"];
	        this.infoUrl = source["infoUrl"];
	    }
	}

}

export namespace vpnprov {
	
	export class Field {
	    key: string;
	    label: string;
	    kind: string;
	    help?: string;
	
	    static createFrom(source: any = {}) {
	        return new Field(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.label = source["label"];
	        this.kind = source["kind"];
	        this.help = source["help"];
	    }
	}
	export class Info {
	    id: string;
	    name: string;
	    fields: Field[];
	    portForwarding: boolean;
	    note?: string;
	
	    static createFrom(source: any = {}) {
	        return new Info(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.fields = this.convertValues(source["fields"], Field);
	        this.portForwarding = source["portForwarding"];
	        this.note = source["note"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Region {
	    id: string;
	    name: string;
	    portForward: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Region(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.portForward = source["portForward"];
	    }
	}

}

