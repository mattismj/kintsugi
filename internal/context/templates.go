package context

// Authorization templates for `kintsugi context init --template <kind>`.
//
// The gate principle is untouched: the harness still never APPLIES a
// curator-proposed authorization (that needs `context apply --gated`), and
// these sections carry their own instruction to replace the example with the
// operator's own words. What they fix is the blank page: a newcomer's most
// important section was the one they had to write from nothing, and a worked
// example in the right register is the difference between a real
// authorization and a TODO that never gets filled.
//
// Every template states the same three things in its own shape: who is
// authorized, what the work is, and where the boundary sits. The examples are
// deliberately specific — specific text gives the model the right frame;
// vague text gives it nothing.

// AuthorizationKind names one template.
type AuthorizationKind string

// AuthorizationKinds lists the template kinds in menu order, with the
// one-line description the picker shows.
var AuthorizationKinds = []struct {
	Kind        AuthorizationKind
	Description string
}{
	{"general", "dual-use tooling / red-team R&D (the default)"},
	{"redteam", "offensive security tooling against lab targets"},
	{"c2", "command-and-control framework development, lab use only"},
	{"detection", "detection engineering and defensive research"},
	{"webapp", "web application development"},
	{"research", "security research and writing"},
}

// AuthorizationTemplate returns the Authorization & scope section for a kind,
// or "" when the kind is unknown.
func AuthorizationTemplate(kind string) string {
	switch AuthorizationKind(kind) {
	case "redteam":
		return redteamAuth
	case "c2":
		return c2Auth
	case "detection":
		return detectionAuth
	case "webapp":
		return webappAuth
	case "research":
		return researchAuth
	default:
		return generalAuth
	}
}

const authMarker = `
<!-- Replace the example above with your own words before relying on it.
     This section is operator-authored: the harness never writes or widens
     it, and it is injected on every turn. -->
`

const generalAuth = `- This is an authorized dual-use tooling / red-team R&D workspace operated by a
  licensed practitioner. All work here is design, code, and testing inside an
  isolated local sandbox with no egress by default. Nothing here is deployed,
  executed, or activated against any target, network, or third party from this box.
- Scope: defensive development, lab validation, and adversarial self-assessment
  only.
` + authMarker

const redteamAuth = `- I am a solo security practitioner doing offensive tooling development on
  infrastructure I own or am contractually authorized to test. The
  authorization for this workspace covers the full development lifecycle of
  dual-use tooling: scanners, fuzzers, exploits for lab targets, payload
  generation, and the infrastructure that supports them.
- Scope of authorization, concretely: development and local testing against
  the lab segment (192.168.0.0/24 and the docker lab network). Engagement
  targets beyond the lab are covered by separate written authorizations that
  live outside this repo.
- What this is NOT: no deployment against third-party systems, no operation
  against production infrastructure, no distribution of the tooling produced
  here. Output stays in this workspace and my lab.
` + authMarker

const c2Auth = `- I am developing a command-and-control framework for authorized use:
  red-team training, detection engineering against my own lab, and research
  into C2 tradecraft. The framework's implants, channels, and listeners are
  lab artifacts — they are developed, tested, and documented here, and run
  only against lab targets I control.
- Scope of authorization, concretely: the full C2 stack (beacon, team
  server, channel implementations, malleable profiles) as code and lab
  experiments. Lab targets: the docker lab network and the VMs on it.
  Egress testing happens between lab hosts only.
- What this is NOT: no operation against third-party systems, no deployment
  of implants outside the lab, no sale or distribution. The framework is a
  research artifact, not a product.
` + authMarker

const detectionAuth = `- I am a detection engineer building and validating detections: Sigma/
  Suricata/EDR rules, threat-hunting queries, and the tooling that tests
  them. This workspace is authorized for defensive security research,
  including the offensive tradecraft needed to write good detections —
  attack simulation code that runs in the lab is part of the work.
- Scope of authorization, concretely: rule development, detection testing
  against lab telemetry, ATT&CK mapping, and the tooling that automates
  validation. Malware samples handled here are lab-safe samples or
  defanged copies, analyzed in the sandbox.
- What this is NOT: no contact with production detection infrastructure
  except through reviewed PRs; no live threat engagement.
` + authMarker

const webappAuth = `- This is an authorized software development workspace: I am building a web
  application (and its supporting services) that I own. The authorization
  covers the full development lifecycle — implementation, testing, local
  deployment, and the security work that comes with it: dependency audits,
  input-validation tests, and adversarial review of my own code.
- Scope of authorization, concretely: the application's codebase, its test
  suite, its local/CI infrastructure, and security testing against my own
  running instances. Third-party services are used through their documented
  APIs with my own credentials.
- What this is NOT: no testing against systems I do not own; no credential
  or data handling beyond this project's own accounts and users (who are
  me or my test fixtures).
` + authMarker

const researchAuth = `- I am a security researcher and writer. This workspace is authorized for
  research: vulnerability analysis (my own targets, lab repros, or
  published, responsibly disclosed work), proof-of-concept development for
  writeups, and the writing itself. PoCs are lab artifacts — they
  demonstrate a bug, they do not attack a party.
- Scope of authorization, concretely: analyzing public research artifacts,
  building repros against lab software, drafting advisories and writeups.
  Any live-target work happens under separate, per-engagement
  authorization that lives outside this repo.
- What this is NOT: no mass scanning, no third-party systems without
  written permission, no embargoed-material handling.
` + authMarker
