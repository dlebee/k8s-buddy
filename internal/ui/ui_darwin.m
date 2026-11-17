// Native macOS shell: a transparent always-on-top character, a popover-like alert panel,
// an overview window and a menu bar item. All content is HTML served by the Go process.
// Compiled without ARC; globals hold +1 references for the app's lifetime.

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include "ui_darwin.h"
#include "_cgo_export.h"

// Must match the layout in web/character.html.
static const CGFloat kCharW = 280, kCharH = 200, kHitW = 120, kHitH = 140;
static const CGFloat kPanelW = 400, kPanelH = 580;

static NSString *gCharURL, *gPanelURL, *gOverviewURL;
static NSPanel *gCharWin, *gPanelWin;
static NSWindow *gOverviewWin;
static WKWebView *gOverviewView;
static NSStatusItem *gStatus;
static NSMenu *gMenu;
static BOOL gDragging;
static BOOL gInspectable;
static NSTimeInterval gPanelAutoHiddenAt;

static void togglePanel(void);
static void showOverview(NSString *tab);
static void setCharacterVisible(BOOL visible, BOOL notify);

static NSRect hitRectOnScreen(void) {
    NSRect f = gCharWin.frame;
    return NSMakeRect(f.origin.x + kCharW - kHitW, f.origin.y, kHitW, kHitH);
}

@interface BuddyPanel : NSPanel
@end
@implementation BuddyPanel
- (BOOL)canBecomeKeyWindow { return YES; }
@end

// Sits over the character and turns mouse input into drag / click / context menu.
@interface HitView : NSView
@end
@implementation HitView
- (BOOL)acceptsFirstMouse:(NSEvent *)e { return YES; }
- (void)mouseDown:(NSEvent *)event {
    NSWindow *w = self.window;
    NSPoint start = [NSEvent mouseLocation];
    NSPoint origin = w.frame.origin;
    BOOL dragged = NO;
    gDragging = YES;
    while (YES) {
        NSEvent *e = [w nextEventMatchingMask:(NSEventMaskLeftMouseDragged | NSEventMaskLeftMouseUp)];
        NSPoint p = [NSEvent mouseLocation];
        CGFloat dx = p.x - start.x, dy = p.y - start.y;
        if (!dragged && (fabs(dx) > 3 || fabs(dy) > 3)) dragged = YES;
        if (dragged) {
            [w setFrameOrigin:NSMakePoint(origin.x + dx, origin.y + dy)];
            if (gPanelWin.isVisible) [gPanelWin orderOut:nil];
        }
        if (e.type == NSEventTypeLeftMouseUp) break;
    }
    gDragging = NO;
    if (dragged) {
        [w saveFrameUsingName:w.frameAutosaveName];
    } else {
        togglePanel();
    }
}
- (void)rightMouseDown:(NSEvent *)event {
    [NSMenu popUpContextMenu:gMenu withEvent:event forView:self];
}
@end

@interface BuddyController : NSObject <NSApplicationDelegate, NSMenuDelegate, NSWindowDelegate>
@end

@implementation BuddyController
- (void)applicationWillTerminate:(NSNotification *)n { goBuddyWillTerminate(); }
- (void)showAlerts:(id)sender { togglePanel(); }
- (void)showOverview:(id)sender { showOverview(nil); }
- (void)showSettings:(id)sender { showOverview(@"settings"); }
- (void)toggleCharacter:(id)sender { setCharacterVisible(!gCharWin.isVisible, YES); }
- (void)reloadOverview:(id)sender { [gOverviewView reload]; }
- (void)quit:(id)sender { [NSApp terminate:nil]; }
- (void)menuNeedsUpdate:(NSMenu *)menu {
    [[menu itemWithTag:10] setTitle:(gCharWin.isVisible ? @"Hide Buddy" : @"Show Buddy")];
}
- (void)windowDidResignKey:(NSNotification *)n {
    if (n.object == gPanelWin && gPanelWin.isVisible) {
        [gPanelWin orderOut:nil];
        gPanelAutoHiddenAt = [NSDate timeIntervalSinceReferenceDate];
    }
}
// Only let the character catch the mouse over the character itself, so the rest of its
// transparent window never blocks clicks meant for apps underneath.
- (void)trackMouse:(NSTimer *)t {
    if (!gCharWin.isVisible || gDragging) return;
    BOOL inside = NSPointInRect([NSEvent mouseLocation], hitRectOnScreen());
    if (gCharWin.ignoresMouseEvents == inside) gCharWin.ignoresMouseEvents = !inside;
}
@end

static BuddyController *gController;

// Keeps every web view on the app's own local server. The views have no address bar, so a page
// from anywhere else (a link that slipped into cluster data, a port-forwarded app) would look
// like part of k8s-buddy; such navigations are cancelled. Links meant for the browser go through
// the API (ingress-open, port-forward open), which opens them in the default browser.
@interface BuddyNavigation : NSObject <WKNavigationDelegate>
@end
@implementation BuddyNavigation
- (void)webView:(WKWebView *)wv decidePolicyForNavigationAction:(WKNavigationAction *)action
    decisionHandler:(void (^)(WKNavigationActionPolicy))decide {
    NSURL *u = action.request.URL;
    NSURL *home = [NSURL URLWithString:gOverviewURL];
    BOOL local = [u.scheme isEqualToString:@"http"] && home.host != nil && [u.host isEqualToString:home.host] &&
                 home.port != nil && [u.port isEqualToNumber:home.port];
    BOOL blank = [u.absoluteString isEqualToString:@"about:blank"];
    decide((local || blank) ? WKNavigationActionPolicyAllow : WKNavigationActionPolicyCancel);
}
@end

static BuddyNavigation *gNavigation; // web views hold their navigation delegate weakly

static WKWebView *makeWebView(NSRect frame, NSString *url, BOOL transparent) {
    WKWebViewConfiguration *cfg = [[[WKWebViewConfiguration alloc] init] autorelease];
    WKWebView *wv = [[WKWebView alloc] initWithFrame:frame configuration:cfg];
    if (!gNavigation) gNavigation = [[BuddyNavigation alloc] init];
    wv.navigationDelegate = gNavigation;
    wv.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    if (transparent) [wv setValue:@NO forKey:@"drawsBackground"];
    if (@available(macOS 13.3, *)) wv.inspectable = gInspectable;
    [wv loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:url]]];
    return wv;
}

static NSPanel *makeFloatingPanel(NSRect frame) {
    NSPanel *p = [[BuddyPanel alloc] initWithContentRect:frame
                                               styleMask:(NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel)
                                                 backing:NSBackingStoreBuffered
                                                   defer:NO];
    p.opaque = NO;
    p.backgroundColor = NSColor.clearColor;
    p.hasShadow = NO;
    p.level = NSFloatingWindowLevel;
    p.hidesOnDeactivate = NO;
    p.releasedWhenClosed = NO;
    p.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces | NSWindowCollectionBehaviorFullScreenAuxiliary |
                           NSWindowCollectionBehaviorStationary | NSWindowCollectionBehaviorIgnoresCycle;
    return p;
}

static BOOL onSomeScreen(NSRect f) {
    NSPoint c = NSMakePoint(NSMidX(f), NSMidY(f));
    for (NSScreen *s in NSScreen.screens) {
        if (NSPointInRect(c, s.visibleFrame)) return YES;
    }
    return NO;
}

static void createCharacter(void) {
    NSRect vf = NSScreen.mainScreen.visibleFrame;
    NSRect frame = NSMakeRect(NSMaxX(vf) - kCharW - 16, vf.origin.y + 16, kCharW, kCharH);
    gCharWin = makeFloatingPanel(frame);
    gCharWin.ignoresMouseEvents = YES;
    NSView *content = gCharWin.contentView;
    [content addSubview:makeWebView(content.bounds, gCharURL, YES)];
    HitView *hit = [[HitView alloc] initWithFrame:NSMakeRect(kCharW - kHitW, 0, kHitW, kHitH)];
    hit.autoresizingMask = NSViewMinXMargin | NSViewMaxYMargin;
    [content addSubview:hit];
    [gCharWin setFrameAutosaveName:@"K8sBuddyCharacter"];
    if (!onSomeScreen(gCharWin.frame)) [gCharWin setFrame:frame display:NO];
    NSTimer *t = [NSTimer timerWithTimeInterval:0.05 target:gController selector:@selector(trackMouse:) userInfo:nil repeats:YES];
    [[NSRunLoop mainRunLoop] addTimer:t forMode:NSRunLoopCommonModes];
}

static void createPanel(void) {
    gPanelWin = makeFloatingPanel(NSMakeRect(0, 0, kPanelW, kPanelH));
    gPanelWin.level = NSPopUpMenuWindowLevel;
    gPanelWin.delegate = gController;
    NSView *content = gPanelWin.contentView;
    [content addSubview:makeWebView(content.bounds, gPanelURL, YES)];
}

static void positionPanel(void) {
    NSRect anchor;
    NSScreen *screen;
    BOOL fromTray = !gCharWin.isVisible;
    if (fromTray) {
        anchor = gStatus.button.window.frame;
        screen = gStatus.button.window.screen ?: NSScreen.mainScreen;
    } else {
        anchor = hitRectOnScreen();
        screen = gCharWin.screen ?: NSScreen.mainScreen;
    }
    NSRect vf = screen.visibleFrame;
    CGFloat x, y;
    if (fromTray) {
        x = NSMidX(anchor) - kPanelW / 2;
        y = NSMinY(anchor) - kPanelH;
    } else {
        x = NSMinX(anchor) - kPanelW + 14;
        if (x < NSMinX(vf)) x = NSMaxX(anchor) - 14;
        y = NSMinY(anchor);
    }
    x = MAX(NSMinX(vf), MIN(x, NSMaxX(vf) - kPanelW));
    y = MAX(NSMinY(vf), MIN(y, NSMaxY(vf) - kPanelH));
    [gPanelWin setFrame:NSMakeRect(x, y, kPanelW, kPanelH) display:NO];
}

static void togglePanel(void) {
    if (gPanelWin.isVisible) {
        [gPanelWin orderOut:nil];
        return;
    }
    if ([NSDate timeIntervalSinceReferenceDate] - gPanelAutoHiddenAt < 0.3) return;
    positionPanel();
    [gPanelWin makeKeyAndOrderFront:nil];
}

static void showOverview(NSString *tab) {
    if (gPanelWin.isVisible) [gPanelWin orderOut:nil];
    if (!gOverviewWin) {
        NSRect frame = NSMakeRect(0, 0, 1280, 780);
        gOverviewWin = [[NSWindow alloc] initWithContentRect:frame
                                                   styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
                                                              NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable)
                                                     backing:NSBackingStoreBuffered
                                                       defer:NO];
        gOverviewWin.title = @"k8s-buddy";
        gOverviewWin.releasedWhenClosed = NO;
        gOverviewWin.backgroundColor = [NSColor colorWithRed:0.043 green:0.055 blue:0.078 alpha:1];
        gOverviewWin.minSize = NSMakeSize(720, 420);
        [gOverviewWin center];
        [gOverviewWin setFrameAutosaveName:@"K8sBuddyOverview"];
        NSString *url = tab.length ? [NSString stringWithFormat:@"%@#%@", gOverviewURL, tab] : gOverviewURL;
        gOverviewView = makeWebView(gOverviewWin.contentView.bounds, url, NO);
        [gOverviewWin.contentView addSubview:gOverviewView];
    } else if (tab.length) {
        [gOverviewView evaluateJavaScript:[NSString stringWithFormat:@"window.buddyShowTab && buddyShowTab('%@')", tab]
                        completionHandler:nil];
    }
    [NSApp activateIgnoringOtherApps:YES];
    [gOverviewWin makeKeyAndOrderFront:nil];
}

static void setCharacterVisible(BOOL visible, BOOL notify) {
    if (visible == gCharWin.isVisible) return;
    if (visible) {
        [gCharWin orderFrontRegardless];
    } else {
        [gCharWin orderOut:nil];
        if (gPanelWin.isVisible) [gPanelWin orderOut:nil];
    }
    if (notify) goBuddyCharacterVisible(visible ? 1 : 0);
}

static NSMenuItem *addItem(NSMenu *m, NSString *title, SEL action, NSString *key) {
    NSMenuItem *it = [m addItemWithTitle:title action:action keyEquivalent:key];
    it.target = gController;
    return it;
}

static void createMenus(void) {
    gMenu = [[NSMenu alloc] initWithTitle:@"k8s-buddy"];
    gMenu.delegate = gController;
    addItem(gMenu, @"Alerts…", @selector(showAlerts:), @"");
    addItem(gMenu, @"Multi-cluster Overview", @selector(showOverview:), @"o");
    [gMenu addItem:[NSMenuItem separatorItem]];
    addItem(gMenu, @"Hide Buddy", @selector(toggleCharacter:), @"").tag = 10;
    addItem(gMenu, @"Settings…", @selector(showSettings:), @",");
    [gMenu addItem:[NSMenuItem separatorItem]];
    addItem(gMenu, @"Quit k8s-buddy", @selector(quit:), @"q");

    gStatus = [[[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength] retain];
    gStatus.button.title = @"⎈";
    gStatus.button.toolTip = @"k8s-buddy";
    gStatus.menu = gMenu;

    // Hidden main menu so standard shortcuts (copy/paste, close, quit) work in our windows.
    NSMenu *main = [[NSMenu alloc] init];
    NSMenuItem *appItem = [main addItemWithTitle:@"" action:nil keyEquivalent:@""];
    NSMenu *app = [[NSMenu alloc] init];
    [app addItemWithTitle:@"Quit k8s-buddy" action:@selector(terminate:) keyEquivalent:@"q"];
    appItem.submenu = app;
    NSMenuItem *editItem = [main addItemWithTitle:@"" action:nil keyEquivalent:@""];
    NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
    [edit addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
    [[edit addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"z"] setKeyEquivalentModifierMask:NSEventModifierFlagCommand | NSEventModifierFlagShift];
    [edit addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
    [edit addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
    [edit addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
    [edit addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
    editItem.submenu = edit;
    NSMenuItem *winItem = [main addItemWithTitle:@"" action:nil keyEquivalent:@""];
    NSMenu *win = [[NSMenu alloc] initWithTitle:@"Window"];
    [win addItemWithTitle:@"Close" action:@selector(performClose:) keyEquivalent:@"w"];
    [win addItemWithTitle:@"Minimize" action:@selector(performMiniaturize:) keyEquivalent:@"m"];
    addItem(win, @"Reload", @selector(reloadOverview:), @"r");
    winItem.submenu = win;
    NSApp.mainMenu = main;
}

void buddy_run(const char *characterURL, const char *panelURL, const char *overviewURL, int showCharacter, int inspectable) {
    gInspectable = inspectable != 0;
    @autoreleasepool {
        gCharURL = [[NSString alloc] initWithUTF8String:characterURL];
        gPanelURL = [[NSString alloc] initWithUTF8String:panelURL];
        gOverviewURL = [[NSString alloc] initWithUTF8String:overviewURL];

        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        gController = [[BuddyController alloc] init];
        NSApp.delegate = gController;

        createMenus();
        createCharacter();
        createPanel();
        if (showCharacter) [gCharWin orderFrontRegardless];
    }
    [NSApp run];
}

void buddy_set_tray(const char *title, const char *tooltip) {
    NSString *t = [[NSString alloc] initWithUTF8String:title];
    NSString *tt = [[NSString alloc] initWithUTF8String:tooltip];
    dispatch_async(dispatch_get_main_queue(), ^{
        gStatus.button.title = t;
        gStatus.button.toolTip = tt;
        [t release];
        [tt release];
    });
}

void buddy_show_overview(const char *tab) {
    NSString *t = [[NSString alloc] initWithUTF8String:tab];
    dispatch_async(dispatch_get_main_queue(), ^{
        showOverview(t);
        [t release];
    });
}

void buddy_toggle_panel(void) {
    dispatch_async(dispatch_get_main_queue(), ^{ togglePanel(); });
}

void buddy_hide_panel(void) {
    dispatch_async(dispatch_get_main_queue(), ^{ [gPanelWin orderOut:nil]; });
}

void buddy_set_character_visible(int visible) {
    dispatch_async(dispatch_get_main_queue(), ^{ setCharacterVisible(visible != 0, NO); });
}

void buddy_quit(void) {
    dispatch_async(dispatch_get_main_queue(), ^{ [NSApp terminate:nil]; });
}
