// cmdg is a command line client to Gmail.
//
// The major reason for its existence is that the Gmail web UI is not
// friendly to proper quoting.
//
// Main benefits over Gmail web:
//   * Really fast. No browser, CSS or javascript getting in the way.
//   * Low bandwidth.
//   * Uses your EDITOR for composing (emacs keys, yay!)
//
// TODO features:
//   * Send all email asynchronously, with a local journal file for
//     when there are network issues.
//   * GPG integration.
//   * Forwarding
//   * ReplyAll
//   * Label management
//   * Label navigation
//   * Refresh list
//   * Mailbox pagination
//   * Abort sending while in emacs mode.
//   * Delayed sending.
//   * Drafts
package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/mail"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	gmail "code.google.com/p/google-api-go-client/gmail/v1"
	"github.com/ThomasHabets/drive-du/lib"
	"github.com/jroimartin/gocui"
	"github.com/nsf/termbox-go"
)

var (
	config      = flag.String("config", "", "Config file.")
	configure   = flag.Bool("configure", false, "Configure OAuth.")
	readonly    = flag.Bool("readonly", false, "When configuring, only acquire readonly permission.")
	editor      = flag.String("editor", "/usr/bin/emacs", "Default editor to use if EDITOR is not set.")
	replyRegex  = flag.String("reply_regexp", `^(Re|Sv|Aw|AW): `, "If subject matches, there's no need to add a Re: prefix.")
	replyPrefix = flag.String("reply_prefix", "Re: ", "String to prepend to subject in replies.")
	signature   = flag.String("signature", "Best regards", "End of all emails.")

	messagesView    *gocui.View
	openMessageView *gocui.View
	bottomView      *gocui.View
	ui              *gocui.Gui

	// State keepers.
	openMessageScrollY int
	messages           *messageList
	labels             = make(map[string]string) // From name to ID.
	openMessage        *gmail.Message

	replyRE      *regexp.Regexp
	sendHeaderRE = regexp.MustCompile(`(?:^|\n)Mode: (\w+)(?:$|\n)`)
)

const (
	scopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	scopeModify   = "https://www.googleapis.com/auth/gmail.modify"
	accessType    = "offline"
	email         = "me"

	vnMessages    = "messages"
	vnOpenMessage = "openMessage"
	vnBottom      = "bottom"

	// Fixed labels.
	inbox  = "INBOX"
	unread = "UNREAD"

	maxLine = 80
	spaces  = " \t\r"
)

func getHeader(m *gmail.Message, header string) string {
	for _, h := range m.Payload.Headers {
		if h.Name == header {
			return h.Value
		}
	}
	return ""
}

type parallel struct {
	chans []<-chan func()
}

func (p *parallel) add(f func(chan<- func())) {
	c := make(chan func())
	go f(c)
	p.chans = append(p.chans, c)
}

func (p *parallel) run() {
	for _, ch := range p.chans {
		f := <-ch
		f()
	}
}

type messageList struct {
	current     int
	marked      map[string]bool
	showDetails bool
	messages    []*gmail.Message
}

func list(g *gmail.Service) *messageList {
	res, err := g.Users.Messages.List(email).
		//		LabelIds().
		//		PageToken().
		MaxResults(20).
		//Fields("messages(id,payload,snippet,raw,sizeEstimate),resultSizeEstimate").
		Fields("messages,resultSizeEstimate").
		Q("in:inbox").
		Do()
	if err != nil {
		log.Fatalf("Listing: %v", err)
	}
	fmt.Fprintf(messagesView, "Total size: %d\n", res.ResultSizeEstimate)
	p := parallel{}
	ret := &messageList{
		marked: make(map[string]bool),
	}
	for _, m := range res.Messages {
		m2 := m
		p.add(func(ch chan<- func()) {
			mres, err := g.Users.Messages.Get(email, m2.Id).Format("full").Do()
			if err != nil {
				log.Fatalf("Get message: %v", err)
			}
			ch <- func() {
				ret.messages = append(ret.messages, mres)
			}
		})
	}
	p.run()
	return ret
}

func hasLabel(labels []string, needle string) bool {
	for _, l := range labels {
		if l == needle {
			return true
		}
	}
	return false
}

func parseTime(s string) (time.Time, error) {
	var t time.Time
	var err error
	for _, layout := range []string{
		"Mon, 2 Jan 2006 15:04:05 -0700",
		time.RFC1123Z,
	} {
		t, err = time.Parse(layout, s)
		if err == nil {
			break
		}
	}
	return t, err
}

func timestring(m *gmail.Message) string {
	s := getHeader(m, "Date")
	ts, err := parseTime(s)
	if err != nil {
		return "Unknown"
	}
	if time.Since(ts) > 365*24*time.Hour {
		return ts.Format("2006")
	}
	if time.Now().Day() != ts.Day() {
		return ts.Format("Jan 02")
	}
	return ts.Format("15:04")
}

func fromString(m *gmail.Message) string {
	s := getHeader(m, "From")
	a, err := mail.ParseAddress(s)
	if err != nil {
		return s
	}
	if len(a.Name) > 0 {
		return a.Name
	}
	return a.Address
}

func (l *messageList) draw() {
	messagesView.Clear()
	fromMax := 20
	tsWidth := 7
	for n, m := range l.messages {
		s := fmt.Sprintf(" %+*s | %+*s | %s",
			tsWidth, timestring(m),
			fromMax, fromString(m),
			getHeader(m, "Subject"))
		if l.marked[m.Id] {
			s = "X" + s
		} else if hasLabel(m.LabelIds, unread) {
			s = ">" + s
		} else {
			s = " " + s
		}
		if n == l.current {
			s = "*" + s
		} else {
			s = " " + s
		}
		fmt.Fprint(messagesView, s)
		if n == l.current && l.showDetails {
			fmt.Fprintf(messagesView, "    %s", m.Snippet)
		}
	}
	ui.Flush()
}

func (l *messageList) next() {
	if l.current < len(l.messages)-1 {
		l.current++
	}
}
func (l *messageList) prev() {
	if l.current > 0 {
		l.current--
	}
}
func (l *messageList) fixCurrent() {
	if l.current >= len(l.messages) {
		l.current = len(l.messages) - 1
	}
	if l.current < 0 {
		l.current = 0
	}
}
func (l *messageList) details() {
	l.showDetails = !l.showDetails
}

func getLabels(g *gmail.Service) {
	res, err := g.Users.Labels.List(email).Do()
	if err != nil {
		log.Fatalf("listing labels: %v", err)
	}
	for _, l := range res.Labels {
		labels[l.Name] = l.Id
	}
}

func refreshMessages(g *gmail.Service) {
	marked := make(map[string]bool)
	current := 0
	if messages != nil {
		current = messages.current
		marked = messages.marked
	}
	messages = list(g)
	if marked != nil {
		messages.current = current
		messages.marked = marked
		messages.fixCurrent()
	}
	messages.draw()
}

func quit(g *gocui.Gui, v *gocui.View) error {
	return gocui.ErrorQuit
}
func next(g *gocui.Gui, v *gocui.View) error {
	messages.next()
	messages.draw()
	return nil
}

func mimeDecode(s string) (string, error) {
	s = strings.Replace(s, "-", "+", -1)
	s = strings.Replace(s, "_", "/", -1)
	data, err := base64.StdEncoding.DecodeString(s)
	return string(data), err
}

func mimeEncode(s string) string {
	s = base64.StdEncoding.EncodeToString([]byte(s))
	s = strings.Replace(s, "+", "-", -1)
	s = strings.Replace(s, "/", "_", -1)
	return s
}

func status(s string, args ...interface{}) {
	bottomView.Clear()
	fmt.Fprintf(bottomView, s, args...)
}

func getBody(m *gmail.Message) string {
	if len(m.Payload.Parts) == 0 {
		data, err := mimeDecode(string(m.Payload.Body.Data))
		if err != nil {
			return fmt.Sprintf("TODO Content error: %v", err)
		}
		return data
	}
	for _, p := range m.Payload.Parts {
		if p.MimeType == "text/plain" {
			data, err := mimeDecode(p.Body.Data)
			if err != nil {
				return fmt.Sprintf("TODO Content error: %v", err)
			}
			return string(data)
		}
	}
	return "TODO Unknown data"
}

func messagesCmdRefresh(g *gocui.Gui, v *gocui.View) error {
	refreshMessages(gmailService)
	return nil
}

func messagesCmdOpen(g *gocui.Gui, v *gocui.View) error {
	openMessageScrollY = 0
	openMessageDraw(g, v)
	return nil
}

func messagesCmdMark(g *gocui.Gui, v *gocui.View) error {
	messages.marked[messages.messages[messages.current].Id] = !messages.marked[messages.messages[messages.current].Id]
	return next(g, v)
}

var gmailService *gmail.Service

func messagesCmdArchive(g *gocui.Gui, v *gocui.View) error {
	return messagesCmdApply(g, v, "archiving", func(id string) error {
		_, err := gmailService.Users.Messages.Modify(email, id, &gmail.ModifyMessageRequest{
			RemoveLabelIds: []string{inbox},
		}).Do()
		return err
	})
}

func messagesCmdApply(g *gocui.Gui, v *gocui.View, verb string, f func(string) error) error {
	status("%s emails, please wait...", verb)
	ui.Flush()
	p := parallel{}
	var errstr string
	var ok, fail int
	for _, m := range messages.messages {
		id := m.Id
		if !messages.marked[id] {
			continue
		}
		p.add(func(ch chan<- func()) {
			err := f(id)
			if err != nil {
				ch <- func() {
					errstr = fmt.Sprintf("Error %s %q: %v", verb, id, err)
					fail++
				}
			} else {
				ch <- func() {
					delete(messages.marked, id)
					ok++
				}
			}
		})
	}
	p.run()
	if fail > 0 {
		status("%d %s OK, %d failed: %s", ok, verb, fail, errstr)
	} else {
		messages.marked = make(map[string]bool)
		status("OK, %s %d messages", verb, ok)
	}
	refreshMessages(gmailService)
	messages.draw()
	return nil
}

func messagesCmdDelete(g *gocui.Gui, v *gocui.View) error {
	return messagesCmdApply(g, v, "trashing", func(id string) error {
		_, err := gmailService.Users.Messages.Trash(email, id).Do()
		return err
	})
}

func openMessageCmdMark(g *gocui.Gui, v *gocui.View) error {
	messages.marked[messages.messages[messages.current].Id] = !messages.marked[messages.messages[messages.current].Id]
	return openMessageCmdNext(g, v)
}

func getReply() (string, error) {
	f := &bytes.Buffer{}
	fmt.Fprintf(f, "On %s, %s said:\n", getHeader(openMessage, "Date"), getHeader(openMessage, "From"))
	for _, line := range strings.Split(getBody(openMessage), "\n") {
		line = strings.TrimRight(line, spaces)
		if len(line) > maxLine {
			for n := 0; len(line) > maxLine; n++ {
				fmt.Fprintf(f, "> %s\n", strings.TrimRight(line[:maxLine], spaces))
				line = strings.TrimLeft(line[maxLine:], spaces)
			}
		}
		if len(line) == 0 {
			fmt.Fprintf(f, ">\n")
		} else {
			fmt.Fprintf(f, "> %s\n", line)
		}
	}
	return runEditor(f.String())
}

func runEditor(input string) (string, error) {
	f, err := ioutil.TempFile("", "cmdg-")
	if err != nil {
		return "", fmt.Errorf("creating tempfile: %v", err)
	}
	f.Close()
	defer os.Remove(f.Name())

	if err := ioutil.WriteFile(f.Name(), []byte(input), 0600); err != nil {
		return "", err
	}

	// Restore terminal for editors use.
	termbox.Close()
	defer func() {
		if err := termbox.Init(); err != nil {
			log.Fatalf("termbox failed to re-init: %v", err)
		}
	}()

	// Run editor.
	bin := *editor
	if e := os.Getenv("EDITOR"); len(e) > 0 {
		bin = e
	}
	cmd := exec.Command(bin, f.Name())
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to open editor %q: %v", bin, err)
	}

	// Read back reply.
	data, err := ioutil.ReadFile(f.Name())
	if err != nil {
		return "", fmt.Errorf("reading back editor output: %v", err)
	}
	return string(data), nil
}

func runEditorMode(input string) (string, string, error) {
	var s, mode string
	for {
		var err error
		s, err = runEditor(input)
		if err != nil {
			status("Running editor failed: %v", err)
			return "", "", err
		}
		s2 := strings.SplitN(s, "\n\n", 2)
		if len(s2) != 2 {
			status("Malformed email, reopening editor")
			input = s
			continue
		}
		m := sendHeaderRE.FindStringSubmatch(s2[0])
		if len(m) != 2 {
			status("Sending mode not present in %q, trying again", s2[0])
			input = s
			continue
		}
		mode = strings.ToLower(m[1])
		switch mode {
		case "send":
		case "draft":
		case "abort":
			status("Sending aborted")
			return mode, s, nil
		default:
			input = s
			continue
		}
		break
	}
	return mode, s, nil
}

func messagesCmdCompose(g *gocui.Gui, v *gocui.View) error {
	status("Running editor")
	input := "To: \nSubject: \nMode: Send\n\n" + *signature
	mode, s, err := runEditorMode(input)
	if err != nil {
		status("Running editor: %v", err)
		return nil
	}

	switch mode {
	case "send":
		if _, err := gmailService.Users.Messages.Send(email, &gmail.Message{Raw: mimeEncode(s)}).Do(); err != nil {
			status("Error sending: %v", err)
			return nil
		}
		status("Successfully sent")
	case "draft":
		// TODO
	}
	return nil
}

func openMessageCmdReply(g *gocui.Gui, v *gocui.View) error {
	status("Composing reply")
	body, err := getReply()
	g.Flush()
	if err != nil {
		status("Error creating reply: %v", err)
		return nil
	}

	subject := getHeader(openMessage, "Subject")
	if !replyRE.MatchString(subject) {
		subject = *replyPrefix + subject
	}

	if _, err := gmailService.Users.Messages.Send(email, &gmail.Message{
		Raw: mimeEncode(fmt.Sprintf(`To: %s
Subject: %s

%s`, getHeader(openMessage, "From"), subject, body)),
	}).Do(); err != nil {
		status("Error sending reply: %v", err)
		return nil
	}
	status("Successfully sent reply")
	return nil
}

func openMessageDraw(g *gocui.Gui, v *gocui.View) {
	openMessage = messages.messages[messages.current]
	go func() {
		if !hasLabel(openMessage.LabelIds, unread) {
			return
		}
		id := openMessage.Id
		_, err := gmailService.Users.Messages.Modify(email, id, &gmail.ModifyMessageRequest{
			RemoveLabelIds: []string{unread},
		}).Do()
		if err != nil {
			// TODO: log to file or something.
		}
	}()

	g.Flush()
	openMessageView.Clear()
	w, h := openMessageView.Size()

	bodyLines := strings.Split(getBody(openMessage), "\n")
	maxScroll := len(bodyLines) - h + 10
	if openMessageScrollY > maxScroll {
		openMessageScrollY = maxScroll
	}
	if openMessageScrollY < 0 {
		openMessageScrollY = 0
	}
	bodyLines = bodyLines[openMessageScrollY:]
	body := strings.Join(bodyLines, "\n")

	fmt.Fprintf(openMessageView, "Email %d of %d", messages.current+1, len(messages.messages))
	fmt.Fprintf(openMessageView, "From: %s", getHeader(openMessage, "From"))
	fmt.Fprintf(openMessageView, "Date: %s", getHeader(openMessage, "Date"))
	fmt.Fprintf(openMessageView, "Subject: %s", getHeader(openMessage, "Subject"))
	fmt.Fprintf(openMessageView, strings.Repeat("-", w))
	fmt.Fprintf(openMessageView, "%s", body)
	fmt.Fprintf(openMessageView, "%+v", *openMessage.Payload)
	for _, p := range openMessage.Payload.Parts {
		fmt.Fprintf(openMessageView, "%+v", *p)
	}
	g.SetCurrentView(vnOpenMessage)
}

func openMessageCmdPrev(g *gocui.Gui, v *gocui.View) error {
	openMessageScrollY = 0
	messages.prev()
	openMessageDraw(g, v)
	return nil
}
func openMessageCmdNext(g *gocui.Gui, v *gocui.View) error {
	openMessageScrollY = 0
	messages.next()
	openMessageDraw(g, v)
	return nil
}
func openMessageCmdPageDown(g *gocui.Gui, v *gocui.View) error {
	_, h := openMessageView.Size()
	openMessageScrollY += h
	openMessageDraw(g, v)
	return nil
}
func openMessageCmdScrollDown(g *gocui.Gui, v *gocui.View) error {
	openMessageScrollY += 2
	openMessageDraw(g, v)
	return nil
}
func openMessageCmdPageUp(g *gocui.Gui, v *gocui.View) error {
	_, h := openMessageView.Size()
	openMessageScrollY -= h
	openMessageDraw(g, v)
	return nil
}
func openMessageCmdScrollUp(g *gocui.Gui, v *gocui.View) error {
	openMessageScrollY -= 2
	openMessageDraw(g, v)
	return nil
}

func openMessageCmdClose(g *gocui.Gui, v *gocui.View) error {
	openMessage = nil
	g.SetCurrentView(vnMessages)
	messages.draw()
	return nil
}

func prev(g *gocui.Gui, v *gocui.View) error {
	messages.prev()
	messages.draw()
	return nil
}
func details(g *gocui.Gui, v *gocui.View) error {
	messages.details()
	messages.draw()
	return nil
}

func layout(g *gocui.Gui) error {
	maxX, maxY := g.Size()
	_ = maxY
	var err error
	create := false
	if messagesView == nil {
		create = true
	}
	messagesView, err = g.SetView(vnMessages, -1, -1, maxX, maxY-2)
	if err != nil {
		if create != (err == gocui.ErrorUnkView) {
			return err
		}
	}
	bottomView, err = g.SetView(vnBottom, -1, maxY-2, maxX, maxY)
	if err != nil {
		if create != (err == gocui.ErrorUnkView) {
			return err
		}
	}
	if openMessage == nil {
		ui.DeleteView(vnOpenMessage)
	} else {
		openMessageView, err = ui.SetView(vnOpenMessage, -1, -1, maxX, maxY-2)
		if err != nil {
			return err
		}
	}
	if create {
		fmt.Fprintf(messagesView, "Loading...")
		status("cmdg")
	}
	return nil
}
func main() {
	flag.Parse()

	var err error
	if replyRE, err = regexp.Compile(*replyRegex); err != nil {
		log.Fatalf("-reply_regexp %q is not a valid regex: %v", *replyRegex, err)
	}
	if *config == "" {
		log.Fatalf("-config required")
	}

	scope := scopeModify
	if *readonly {
		scope = scopeReadonly
	}
	if *configure {
		if err := lib.ConfigureWrite(scope, accessType, *config); err != nil {
			log.Fatalf("Failed to config: %v", err)
		}
		return
	}

	conf, err := lib.ReadConfig(*config)
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}

	t, err := lib.Connect(conf.OAuth, scope, accessType)
	if err != nil {
		log.Fatalf("Failed to connect to gmail: %v", err)
	}
	g, err := gmail.New(t.Client())
	if err != nil {
		log.Fatalf("Failed to create gmail client: %v", err)
	}

	ui = gocui.NewGui()
	if err := ui.Init(); err != nil {
		log.Panicln(err)
	}
	defer ui.Close()
	ui.SetLayout(layout)

	// Global keys.
	for key, cb := range map[interface{}]func(g *gocui.Gui, v *gocui.View) error{
		gocui.KeyCtrlC: quit,
		'q':            quit,
	} {
		if err := ui.SetKeybinding("", key, 0, cb); err != nil {
			log.Fatalf("Bind %v: %v", key, err)
		}
	}

	// Message list keys.
	for key, cb := range map[interface{}]func(g *gocui.Gui, v *gocui.View) error{
		gocui.KeyTab:   details,
		gocui.KeyCtrlP: prev,
		'p':            prev,
		gocui.KeyCtrlN: next,
		'n':            next,
		gocui.KeyCtrlR: messagesCmdRefresh,
		'r':            messagesCmdRefresh,
		'x':            messagesCmdMark,
		'\n':           messagesCmdOpen,
		'\r':           messagesCmdOpen,
		gocui.KeyCtrlM: messagesCmdOpen,
		gocui.KeyCtrlJ: messagesCmdOpen,
		'>':            messagesCmdOpen,
		'd':            messagesCmdDelete,
		'a':            messagesCmdArchive,
		'e':            messagesCmdArchive,
		'c':            messagesCmdCompose,
	} {
		if err := ui.SetKeybinding(vnMessages, key, 0, cb); err != nil {
			log.Fatalf("Bind %v: %v", key, err)
		}
	}

	// Open message read.
	for key, cb := range map[interface{}]func(g *gocui.Gui, v *gocui.View) error{
		'<':                 openMessageCmdClose,
		'p':                 openMessageCmdScrollUp,
		'n':                 openMessageCmdScrollDown,
		'x':                 openMessageCmdMark,
		'r':                 openMessageCmdReply,
		gocui.KeyCtrlP:      openMessageCmdPrev,
		gocui.KeyCtrlN:      openMessageCmdNext,
		gocui.KeySpace:      openMessageCmdPageDown,
		gocui.KeyPgdn:       openMessageCmdPageDown,
		gocui.KeyBackspace:  openMessageCmdPageUp,
		gocui.KeyBackspace2: openMessageCmdPageUp,
		gocui.KeyPgup:       openMessageCmdPageUp,
	} {
		if err := ui.SetKeybinding(vnOpenMessage, key, 0, cb); err != nil {
			log.Fatalf("Bind %v: %v", key, err)
		}
	}
	ui.Flush()
	ui.SetCurrentView(vnMessages)
	refreshMessages(g)
	getLabels(g)
	gmailService = g
	err = ui.MainLoop()
	if err != nil && err != gocui.ErrorQuit {
		log.Panicln(err)
	}
}
