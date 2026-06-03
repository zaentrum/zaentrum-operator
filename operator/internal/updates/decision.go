package updates

import "strings"

// Decision is the resolved Stage-2 update outcome for a single reconcile pass.
type Decision struct {
	// RenderTag is the image tag the templates should render with this pass.
	RenderTag string
	// AvailableUpdate is the value to surface in status.availableUpdate: the
	// channel target when it differs from RenderTag, otherwise "".
	AvailableUpdate string
	// Applied is true when this pass moved to the channel target on its own
	// (auto mode picked up a newer tag).
	Applied bool
}

// Decide computes the render tag and surfaced availableUpdate for one pass.
//
// Inputs:
//   - specVersion: spec.version ("" / "latest" => track channel; otherwise pinned).
//   - auto: spec.update.mode == "auto".
//   - channelTarget: tag the resolved channel points at; "" when discovery
//     failed or is unavailable this pass.
//
// Rules (mirroring the Stage-2 contract):
//   - Pinned spec.version always renders as-is; channel discovery is ignored
//     and no update is surfaced (the operator opted out of channel tracking).
//   - Unpinned: the base render tag is the channel target (or "latest" when
//     discovery failed). When auto, we render the channel target directly so
//     the new tag is applied this pass. When manual, we only surface the
//     channel target as availableUpdate if it differs from what we render.
func Decide(specVersion string, auto bool, channelTarget string) Decision {
	target := strings.TrimSpace(channelTarget)

	if IsPinned(specVersion) {
		// Pinned: render exactly the pinned tag, ignore the channel entirely.
		return Decision{RenderTag: strings.TrimSpace(specVersion)}
	}

	// Unpinned: discovery failed -> behave like a plain "latest" render with
	// nothing to surface.
	if target == "" {
		return Decision{RenderTag: EffectiveTag(specVersion, "")}
	}

	if auto {
		// Auto mode rolls to the channel target in this very pass.
		return Decision{RenderTag: target, Applied: true}
	}

	// Manual mode: keep rendering "latest" but surface the concrete channel
	// target as an available update when it is something other than what we
	// render. (With both channels at "latest" today this collapses to "no
	// update", which is correct.)
	rendered := EffectiveTag(specVersion, "")
	d := Decision{RenderTag: rendered}
	if target != rendered {
		d.AvailableUpdate = target
	}
	return d
}
