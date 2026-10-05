package messages

import "strings"

type Messages struct {
	Ticket       TicketMessages       `toml:"ticket"`
	Commands     CommandMessages      `toml:"commands"`
	Errors       ErrorMessages        `toml:"errors"`
	Review       ReviewMessages       `toml:"review"`
	CloseRequest CloseRequestMessages `toml:"close_request"`
	TimedClose   TimedCloseMessages   `toml:"timed_close"`
	System       SystemMessages       `toml:"system"`
	Staff        StaffMessages        `toml:"staff"`
	Note         NoteMessages         `toml:"note"`
}

type SystemMessages struct {
	PanelNoneSelected  string `toml:"panel_none_selected"`
	PanelGuildOnly     string `toml:"panel_guild_only"`
	PanelCreateFailed  string `toml:"panel_create_failed"`
	PanelTicketCreated string `toml:"panel_ticket_created"`
	ModalError         string `toml:"modal_error"`
	MessageNotFound    string `toml:"message_not_found"`
	MessagePermFailed  string `toml:"message_perm_failed"`
	MessageSendFailed  string `toml:"message_send_failed"`
	MessagePosted      string `toml:"message_posted"`
}

type StaffMessages struct {
	DisassocNoAccount    string `toml:"disassoc_no_account"`
	DisassocDbError      string `toml:"disassoc_db_error"`
	DisassocConfirmBody  string `toml:"disassoc_confirm_body"`
	DisassocNoPermission string `toml:"disassoc_no_permission"`
	DisassocInvalidBtn   string `toml:"disassoc_invalid_btn"`
	DisassocAlreadyGone  string `toml:"disassoc_already_gone"`
	DisassocSuccess      string `toml:"disassoc_success"`
	DisassocCancelled    string `toml:"disassoc_cancelled"`
}

type NoteMessages struct {
	Saved         string `toml:"saved"`
	SaveFailed    string `toml:"save_failed"`
	SpecifyUser   string `toml:"specify_user"`
	TicketLoadErr string `toml:"ticket_load_err"`
}

type TimedCloseMessages struct {
	Scheduled        string `toml:"scheduled"`
	AlreadyScheduled string `toml:"already_scheduled"`
	InvalidDelay     string `toml:"invalid_delay"`
	CancelledButton  string `toml:"cancelled_button"`
	CancelledMessage string `toml:"cancelled_message"`
}

type CloseRequestMessages struct {
	CannotConfirmOwn string `toml:"cannot_confirm_own"`
	StaffMustConfirm string `toml:"staff_must_confirm"`
	OwnerMustConfirm string `toml:"owner_must_confirm"`
	NoPending        string `toml:"no_pending"`
	Accepted         string `toml:"accepted"`
}

type TicketMessages struct {
	Created                       string `toml:"created"`
	Closed                        string `toml:"closed"`
	ForceClosed                   string `toml:"force_closed"`
	Archived                      string `toml:"archived"`
	Claimed                       string `toml:"claimed"`
	Unclaimed                     string `toml:"unclaimed"`
	UserAdded                     string `toml:"user_added"`
	UserRemoved                   string `toml:"user_removed"`
	StatusSet                     string `toml:"status_set"`
	NotATicket                    string `toml:"not_a_ticket"`
	OpenSuccess                   string `toml:"open_success"`
	CloseSuccess                  string `toml:"close_success"`
	CloseSuccessNoTranscript      string `toml:"close_success_no_transcript"`
	ForceCloseSuccess             string `toml:"force_close_success"`
	ForceCloseSuccessNoTranscript string `toml:"force_close_success_no_transcript"`
	ForceCloseScheduled           string `toml:"force_close_scheduled"`
	ForceCloseAlreadyScheduled    string `toml:"force_close_already_scheduled"`
	ForceCloseInvalidDelay        string `toml:"force_close_invalid_delay"`
	CloseCancel                   string `toml:"close_cancel"`
	CloseReject                   string `toml:"close_reject"`
	TranscriptDM                  string `toml:"transcript_dm"`
}

type CommandMessages struct {
	ClosePrompt      string `toml:"close_prompt"`
	ForceClosePrompt string `toml:"force_close_prompt"`
	ClaimSuccess     string `toml:"claim_success"`
	UnclaimSuccess   string `toml:"unclaim_success"`
}

type ErrorMessages struct {
	Forbidden    string `toml:"forbidden"`
	NotFound     string `toml:"not_found"`
	BadStatus    string `toml:"bad_status"`
	Internal     string `toml:"internal"`
	UnknownPanel string `toml:"unknown_panel"`
	InvalidID    string `toml:"invalid_id"`
	TicketLimit  string `toml:"ticket_limit"`
}

type ReviewMessages struct {
	Prompt          string `toml:"prompt"`
	ThankYou        string `toml:"thank_you"`
	Skipped         string `toml:"skipped"`
	ScoreOutOfRange string `toml:"score_out_of_range"`
}

func Format(s string, pairs ...string) string {
	if len(pairs)%2 != 0 {
		panic("messages.Format: pairs must be even")
	}
	args := make([]string, 0, len(pairs)*4)
	for i := 0; i < len(pairs); i += 2 {
		args = append(args, "{"+pairs[i]+"}", pairs[i+1], "<<"+strings.ToUpper(pairs[i])+">>", pairs[i+1])
	}
	return strings.NewReplacer(args...).Replace(s)
}
