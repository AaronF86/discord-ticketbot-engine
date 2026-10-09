package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	tb "git.aaronf86.tech/aaronf86/ticketbot/discord"
)

const closeRequestOwnerLayout = `[{"type":10,"content":"<<Closer>>"},{"type":17,"accent_color":4610710,"components":[{"type":10,"content":"## ❓ Can this ticket be closed?\n<<CLOSEREQUESTER>> wants to close this ticket.\nClick \"Accept\" to close it, or \"Reject\" if further assistance is needed.\n\nThe messages in this channel will be archived for future reference."}]}]`

const closeRequestStaffLayout = `[{"type":10,"content":"<<Closer>>"},{"type":17,"accent_color":4610710,"components":[{"type":10,"content":"## ❓ Can this ticket be closed?\nA staff member (<<CLOSEREQUESTER>>) has requested to close this ticket.\nClick \"Accept\" if your issue is resolved, or \"Reject\" if you still need help.\n\nThe messages in this channel will be archived for future reference."}]}]`

func SeedDefaults(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		INSERT INTO discord_templates (id, name, layout_data, required_perm) VALUES
		    ($1, 'Close Request - Owner', $2::jsonb, 'management_message'),
		    ($3, 'Close Request - Staff', $4::jsonb, 'management_message')
		ON CONFLICT (id) DO NOTHING`,
		tb.TemplateCloseRequestOwner, closeRequestOwnerLayout,
		tb.TemplateCloseRequestStaff, closeRequestStaffLayout,
	); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO discord_buttons (action_type, label, style, emoji) VALUES
		    ($1, 'Confirm Close', 3, ''),
		    ($2, 'Deny',          4, ''),
		    ($3, 'Close Ticket',  4, ''),
		    ($4, 'Keep Open',     3, '')
		ON CONFLICT (action_type) DO NOTHING`,
		tb.ButtonConfirmClose, tb.ButtonDeny, tb.ButtonCloseTicket, tb.ButtonKeepOpen,
	)
	return err
}
