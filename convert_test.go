package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type fakeRes struct{ lids map[string]string }

func (f fakeRes) PN(lid types.JID) types.JID {
	if pn, ok := f.lids[lid.User]; ok {
		return types.NewJID(pn, types.DefaultUserServer)
	}
	return types.JID{}
}
func (f fakeRes) Me() types.JID { return types.NewJID("99600000001", types.DefaultUserServer) }

var (
	lidAli = types.NewJID("111111", types.HiddenUserServer)
	pnAli  = types.NewJID("996700000002", types.DefaultUserServer)
	group  = types.NewJID("120363000000000001", types.GroupServer)
	when   = time.Unix(1795400000, 0)
)

func msg(info types.MessageInfo, m *waE2E.Message) *events.Message {
	e := &events.Message{Info: info, RawMessage: m}
	return e.UnwrapRaw()
}

func TestConvert(t *testing.T) {
	res := fakeRes{lids: map[string]string{}}
	learned := map[string]string{}
	learn := func(l, p string) { learned[l] = p }

	t.Run("hidden DM address becomes the phone one and is learned", func(t *testing.T) {
		c := convertMessage(msg(types.MessageInfo{ID: "A1", Timestamp: when, PushName: "Ali",
			MessageSource: types.MessageSource{Chat: lidAli, Sender: lidAli, SenderAlt: pnAli}},
			&waE2E.Message{Conversation: proto.String("привет")}), res, learn)
		r := c.Row
		if r == nil || r.ChatID != "996700000002@c.us" || r.Sender != r.ChatID || r.Body != "привет" || r.Kind != "conversation" || r.SenderName != "Ali" {
			t.Fatalf("%+v", r)
		}
		if learned["111111@lid"] != "996700000002@c.us" {
			t.Fatalf("alias not learned: %v", learned)
		}
	})

	t.Run("own message: no sender, chat from the recipient", func(t *testing.T) {
		c := convertMessage(msg(types.MessageInfo{ID: "A2", Timestamp: when, PushName: "me",
			MessageSource: types.MessageSource{Chat: lidAli, Sender: res.Me(), IsFromMe: true, RecipientAlt: pnAli}},
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("ok"),
				ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("A1")}}}), res, learn)
		r := c.Row
		if r.ChatID != "996700000002@c.us" || r.Sender != "" || !r.FromMe || r.ReplyTo != "A1" || r.SenderName != "" || r.Kind != "extendedTextMessage" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("group: hidden sender resolved through the device map; image caption and key envelope", func(t *testing.T) {
		res := fakeRes{lids: map[string]string{"222222": "996700000003"}}
		c := convertMessage(msg(types.MessageInfo{ID: "G1", Timestamp: when,
			MessageSource: types.MessageSource{Chat: group, Sender: types.NewJID("222222", types.HiddenUserServer), IsGroup: true}},
			&waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{},
				ImageMessage: &waE2E.ImageMessage{Caption: proto.String("счёт"), Mimetype: proto.String("image/jpeg"),
					MediaKey: []byte{1}, DirectPath: proto.String("/x")}}), res, learn)
		r := c.Row
		if r.ChatID != group.String() || r.Sender != "996700000003@c.us" || r.Kind != "imageMessage" || !r.HasMedia ||
			r.MediaType != "image/jpeg" || r.Body != "счёт" || len(r.Media) == 0 {
			t.Fatalf("%+v", r)
		}
		var back waE2E.Message
		if proto.Unmarshal(r.Media, &back) != nil || back.GetImageMessage().GetDirectPath() != "/x" || back.SenderKeyDistributionMessage != nil {
			t.Fatal("media blob must hold only the media part")
		}
	})

	t.Run("ephemeral wrapper is unwrapped; document name kept", func(t *testing.T) {
		c := convertMessage(msg(types.MessageInfo{ID: "D1", Timestamp: when, MessageSource: types.MessageSource{Chat: pnAli, Sender: pnAli}},
			&waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
				DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("акт.pdf"), Mimetype: proto.String("application/pdf")}}}}), res, learn)
		if r := c.Row; r == nil || r.Kind != "documentMessage" || r.MediaName != "акт.pdf" {
			t.Fatalf("%+v", c.Row)
		}
	})

	t.Run("edit, revoke and reaction change earlier rows instead of adding one", func(t *testing.T) {
		src := types.MessageSource{Chat: pnAli, Sender: pnAli}
		e := convertMessage(msg(types.MessageInfo{ID: "E1", Timestamp: when, MessageSource: src},
			&waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A1")},
				EditedMessage: &waE2E.Message{Conversation: proto.String("привет!")}}}}}), res, learn)
		if e.Row != nil || len(e.Ops) != 1 || e.Ops[0].EditID != "A1" || e.Ops[0].EditBody != "привет!" || e.Event != "message.edited" {
			t.Fatalf("edit %+v", e)
		}
		v := convertMessage(msg(types.MessageInfo{ID: "R1", Timestamp: when, MessageSource: src},
			&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("A1")}}}), res, learn)
		if v.Row != nil || v.Ops[0].RevokeID != "A1" {
			t.Fatalf("revoke %+v", v)
		}
		x := convertMessage(msg(types.MessageInfo{ID: "X1", Timestamp: when, MessageSource: types.MessageSource{Chat: group, Sender: res.Me(), IsFromMe: true, IsGroup: true}},
			&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("G1")}, Text: proto.String("👍")}}), res, learn)
		if x.Row != nil || x.Ops[0].ReactID != "G1" || x.Ops[0].Reactor != "99600000001@c.us" || x.Ops[0].Reaction != "👍" {
			t.Fatalf("reaction %+v", x)
		}
	})

	t.Run("control-only messages are not stored", func(t *testing.T) {
		c := convertMessage(msg(types.MessageInfo{ID: "K1", Timestamp: when, MessageSource: types.MessageSource{Chat: group, Sender: pnAli, IsGroup: true}},
			&waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}}), res, learn)
		if c.Row != nil || len(c.Ops) != 0 {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("poll and location become readable text", func(t *testing.T) {
		p := convertMessage(msg(types.MessageInfo{ID: "P1", Timestamp: when, MessageSource: types.MessageSource{Chat: pnAli, Sender: pnAli}},
			&waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("Когда?"),
				Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Пн")}, {OptionName: proto.String("Вт")}}}}), res, learn)
		if p.Row.Body != "Когда?\n- Пн\n- Вт" || p.Row.Kind != "pollCreationMessageV3" {
			t.Fatalf("%q %s", p.Row.Body, p.Row.Kind)
		}
		l := convertMessage(msg(types.MessageInfo{ID: "L1", Timestamp: when, MessageSource: types.MessageSource{Chat: pnAli, Sender: pnAli}},
			&waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: proto.String("Офис"), DegreesLatitude: proto.Float64(42.87), DegreesLongitude: proto.Float64(74.59)}}), res, learn)
		if l.Row.Body != "Офис\n42.870000,74.590000" {
			t.Fatalf("%q", l.Row.Body)
		}
	})
}

func TestChatIDs(t *testing.T) {
	for in, want := range map[string]string{"996700000002@c.us": "996700000002@c.us", "+996700000002": "996700000002@c.us",
		"996700000002@s.whatsapp.net": "996700000002@c.us", "120363000000000001@g.us": "120363000000000001@g.us", "111111@lid": "111111@lid"} {
		j, err := parseChatID(in)
		if err != nil || chatID(j) != want {
			t.Errorf("%s -> %s (%v), want %s", in, chatID(j), err, want)
		}
	}
	if tail("false_996700000002@c.us_3EB0ABC_996700000003@c.us") != "3EB0ABC" || tail("3EB0ABC") != "3EB0ABC" {
		t.Fatal("tail")
	}
	if q := ftsQuery("договор подписан!"); q != `"договор" "подписан"*` {
		t.Fatal(q)
	}
}
