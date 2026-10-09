package core

import "context"

type Action string

const (
	ActionClose        Action = "close"
	ActionForceClose   Action = "force_close"
	ActionOpen         Action = "open"
	ActionAddUser      Action = "add_user"
	ActionRemoveUser   Action = "remove_user"
	ActionClaim        Action = "claim"
	ActionUnclaim      Action = "unclaim"
	ActionSetStatus    Action = "set_status"
	ActionRequestClose Action = "request_close"
	ActionConfirmClose Action = "confirm_close"
)

type Authorizer interface {
	Allow(ctx context.Context, actor string, action Action, ticketID TicketID) (bool, error)
}

type SystemAuthorizer interface {
	SystemAllow(ctx context.Context, actor string, action Action) (bool, error)
}
