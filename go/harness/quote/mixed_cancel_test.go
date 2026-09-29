package quote

import "testing"

func TestMixedCancelGroupKeepsReducerBudgetAndReserve(t *testing.T) {
	for _, reducerFirst := range []bool{false, true} {
		q := NewQueue(maxQueueAge)
		adding := addCancel("A", SideYes)
		reducing := Intent{Market: "A", Side: SideYes, Role: RoleReducing,
			Kind: KindCancelConfirmPlace, Reason: ReasonReduce}
		members := []Intent{adding, reducing}
		if reducerFirst {
			members[0], members[1] = members[1], members[0]
		}
		for _, in := range members {
			mustEnqueue(t, q, 0, in)
		}
		cp := NewCapacity(2, 2)
		cp.Tokens, cp.ReservedTokens = 0, 0
		mustNotDequeue(t, q, running(), cp, "a mixed cancel removes an exit and cannot bypass the bucket")
		cp.BusyGeneral = 1
		cp.ReservedTokens = 1
		d := mustDequeue(t, q, running(), cp)
		if d.Role != RoleReducing || d.Op != OpCancel || len(d.IDs) != 2 ||
			d.Grant.BypassBucket || !d.Grant.ReservedWorker || !d.Grant.ReservedToken {
			t.Fatalf("mixed cancel lost reducer admission: %+v", d)
		}
	}
}
