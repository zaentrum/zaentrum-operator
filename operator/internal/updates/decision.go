package updates

import "strings"

// Latest is the moving tag every image's newest build of main carries. It is
// the one channel target that is not a release: what it serves changes with
// every push, and digest pinning (internal/digest) follows it push by push.
// edge points at it; stable names a release tag.
const Latest = "latest"

// Decision is the resolved Stage-2 update outcome for a single reconcile pass.
type Decision struct {
	// RenderTag is the image tag the templates should render with this pass.
	RenderTag string
	// AvailableUpdate is the value to surface in status.availableUpdate: the
	// channel target when it differs from RenderTag, otherwise "".
	AvailableUpdate string
	// Applied is true when this pass rendered the channel target because
	// auto mode follows it.
	Applied bool
}

// Decide computes the render tag and surfaced availableUpdate for one pass.
//
// Inputs:
//   - specVersion: spec.version ("" / "latest" => follow the channel;
//     anything else pins that tag).
//   - auto: spec.update.mode == "auto".
//   - channelTarget: the tag the channel points at; "" when discovery failed
//     or is unavailable this pass.
//   - current: status.currentVersion, the tag the last pass rendered; "" for
//     an install that has rendered nothing yet.
//
// Rules (the Stage-2 contract):
//   - A pinned spec.version renders as-is; the channel is not consulted and no
//     update is surfaced (the CR opted out of channel tracking).
//   - Auto follows the channel: the channel target is rendered, so a channel
//     that moves to a new release rolls the install in this very pass.
//   - Manual keeps what the install runs and surfaces the channel target as
//     the available update; applying it pins spec.version to it. An install
//     that runs nothing yet starts on the target: a new install on stable runs
//     the release stable names, not latest. A channel whose target is the
//     moving latest (edge) is followed in both modes, as before releases
//     existed: there is no release on it to hold an install at.
//   - Discovery failed: keep what the install runs — a network blip must not
//     move a release install over to latest. An install that runs nothing yet
//     falls back to latest.
func Decide(specVersion string, auto bool, channelTarget, current string) Decision {
	target := strings.TrimSpace(channelTarget)
	running := strings.TrimSpace(current)

	if IsPinned(specVersion) {
		// Pinned: render exactly the pinned tag, ignore the channel entirely.
		return Decision{RenderTag: strings.TrimSpace(specVersion)}
	}

	if target == "" {
		if running != "" {
			return Decision{RenderTag: running}
		}
		return Decision{RenderTag: EffectiveTag(specVersion, "")}
	}

	if auto {
		// Auto mode rolls to the channel target in this very pass.
		return Decision{RenderTag: target, Applied: true}
	}

	// Manual: a new install, or a channel that serves the moving latest, takes
	// the target. Anything else stays where it is and is told what the channel
	// serves.
	if running == "" || target == Latest {
		return Decision{RenderTag: target}
	}
	d := Decision{RenderTag: running}
	if target != running {
		d.AvailableUpdate = target
	}
	return d
}
