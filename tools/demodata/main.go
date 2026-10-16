// Command demodata fills a data directory with made-up proposals (each with
// a PDF paper), contact messages and newsletter subscribers, so that the
// admin pages and exports can be tried without real submissions:
//
//	go run ./tools/demodata -data demo-data
//	ICAS_ADMIN_PASSWORD=choose-one go run . -data demo-data -addr :8091
//
// Everyone in it is fictional: addresses and links use example.* domains and
// IP addresses come from the documentation ranges. The command refuses to
// write into a directory that already holds records, so demo data is never
// mixed with real submissions.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"icas/internal/content"
	"icas/internal/server"
	"icas/internal/store"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("demodata: ")
	dir := flag.String("data", "", "data directory to fill, e.g. demo-data (required; it must not hold records yet)")
	sitePath := flag.String("content", filepath.Join("content", "site.json"), "conference content, for the topics and the submission ID prefix")
	flag.Parse()
	if *dir == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*dir, *sitePath, time.Now()); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Wrote demo data to %s: %d proposals with PDF papers, %d contact messages and %d subscribers.\n",
		*dir, len(proposals), len(contacts), len(subscribers))
}

// run writes all demo records into dir, dated in the weeks before now.
func run(dir, sitePath string, now time.Time) error {
	site, err := content.LoadFile(sitePath)
	if err != nil {
		return err
	}
	for _, name := range []string{"submissions.jsonl", "contacts.jsonl", "subscribers.jsonl", "papers"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%s already holds records; delete it first or choose another directory", dir)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Minute)
	prefix := strings.ToUpper(site.Conference.Acronym + site.Conference.Year)
	if err := writeProposals(st, site, prefix, now); err != nil {
		return err
	}
	for i, m := range contacts {
		m.Received = spread(now, i, len(contacts), 53*time.Hour)
		m.Subject = strings.ReplaceAll(m.Subject, "{W002}", prefix+"-W002")
		m.Message = strings.ReplaceAll(m.Message, "{W002}", prefix+"-W002")
		m.IP = ips[(i+3)%len(ips)]
		if err := add(st, "contacts", m); err != nil {
			return err
		}
	}
	for i, s := range subscribers {
		s.Subscribed = spread(now, i, len(subscribers), 50*time.Hour)
		if err := add(st, "subscribers", s); err != nil {
			return err
		}
	}
	return nil
}

func writeProposals(st *store.Store, site *content.Site, prefix string, now time.Time) error {
	known := map[string]bool{"Other": true}
	for _, t := range site.Topics {
		known[t.Title] = true
	}
	papers := filepath.Join(st.Dir(), "papers")
	if err := os.MkdirAll(papers, 0o700); err != nil {
		return err
	}
	numbers := map[string]int{}
	for i, p := range proposals {
		for _, t := range p.topics {
			if !known[t] {
				return fmt.Errorf("topic %q is not in the conference content; update tools/demodata", t)
			}
		}
		letter := "P"
		if p.track == content.TrackWithWorkshop {
			letter = "W"
		}
		numbers[p.track]++
		sub := p.submission(i)
		sub.ID = fmt.Sprintf("%s-%s%03d", prefix, letter, numbers[p.track])
		sub.Received = spread(now, i, len(proposals), 31*time.Hour)
		if p.track == content.TrackWithWorkshop {
			sub.DemoVideo = "https://video.example.com/" + strings.ToLower(sub.ID)
		}
		// Like the server, name the PDF after the submission ID.
		pdf := paperPDF(site.Conference.FullTitle(), sub)
		if err := os.WriteFile(filepath.Join(papers, sub.ID+".pdf"), pdf, 0o600); err != nil {
			return err
		}
		sum := sha256.Sum256(pdf)
		sub.PaperFile, sub.PaperName = sub.ID+".pdf", p.fileName
		sub.PaperSize, sub.PaperSHA256 = int64(len(pdf)), hex.EncodeToString(sum[:])
		if err := add(st, "submissions", sub); err != nil {
			return err
		}
	}
	return nil
}

// add appends one record to a collection.
func add(st *store.Store, collection string, rec any) error {
	return st.Add(collection, func([]json.RawMessage) (any, error) { return rec, nil })
}

// spread dates the i-th of n records about one step apart, the last one
// shortly before now, so that the records read like recent activity.
func spread(now time.Time, i, n int, step time.Duration) time.Time {
	jitter := time.Duration((i*37)%300) * time.Minute // less than 5 hours, so the order holds
	return now.Add(-time.Duration(n-i)*step + jitter)
}

// proposal is the hand-written part of a demo submission; the rest is
// filled in by submission.
type proposal struct {
	track       string
	title       string
	abstract    string
	topics      []string
	otherTopic  string
	fileName    string // as it was named on the author's computer
	speaker     string
	email       string
	affiliation string
	country     string
	coSpeakers  string

	// Tutorials only.
	plan     [4]string // milestones of the 90-minute workshop
	software string
	targetOS string
	hardware string
	skill    string
	size     string
	laptop   string
	taCount  string
	taNames  string
}

func (p proposal) submission(i int) server.Submission {
	s := server.Submission{
		Track: p.track, Title: p.title, Abstract: p.abstract, Topics: p.topics, OtherTopic: p.otherTopic,
		SpeakerName: p.speaker, SpeakerEmail: p.email, Affiliation: p.affiliation, Country: p.country,
		CoSpeakers: p.coSpeakers,
		Bio: fmt.Sprintf("%s is %s at %s, working on %s. Their recent work appeared at robotics and control venues, "+
			"and they enjoy turning research prototypes into teaching material.",
			p.speaker, roles[i%len(roles)], p.affiliation, strings.ToLower(p.topics[0])),
		Experience: experiences[i%len(experiences)],
		IP:         ips[i%len(ips)],
		UserAgent:  userAgents[i%len(userAgents)],
	}
	if i%3 != 2 {
		handle := p.email[:strings.Index(p.email, "@")]
		s.Links = "https://git.example.org/" + handle + "\nhttps://scholar.example.org/" + handle
	}
	if p.track != content.TrackWithWorkshop {
		s.TalkOutline = "0-1 min: problem and motivation. 1-5 min: method. 5-8 min: results. 8-10 min: limitations and future work."
		return s
	}
	s.TalkOutline = "0-1 min: motivation. 1-4 min: a short demo of what participants will build. 4-5 min: the workshop milestones and prerequisites."
	s.WorkshopPlan = fmt.Sprintf("M1 (0-15 min): %s.\nM2 (15-40 min): %s.\nM3 (40-70 min): %s.\nM4 (70-90 min): %s.",
		capitalize(p.plan[0]), capitalize(p.plan[1]), capitalize(p.plan[2]), capitalize(p.plan[3]))
	s.Software, s.TargetOS, s.Hardware, s.SkillLevel = p.software, p.targetOS, p.hardware, p.skill
	s.WorkshopSize, s.Laptop, s.TACount, s.TANames = p.size, p.laptop, p.taCount, p.taNames
	return s
}

func capitalize(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// ---------------- the paper PDF ----------------

// paperPDF renders an anonymous one-page paper for a submission: the title,
// the abstract, the outline and, with a workshop, its milestones.
func paperPDF(conference string, s server.Submission) []byte {
	track := "Regular Paper"
	if s.Track == content.TrackWithWorkshop {
		track = "Tutorial"
	}
	var pg pdfPage
	pg.text(false, 9, conference+" - "+track)
	pg.off += 10
	for _, line := range wrap(s.Title, 52) {
		pg.text(true, 16, line)
	}
	pg.off += 4
	pg.text(false, 10, "Anonymous submission for double-blind review")
	pg.section("Abstract", s.Abstract)
	pg.section("Presentation outline", s.TalkOutline)
	if s.WorkshopPlan != "" {
		pg.section("Workshop milestones", s.WorkshopPlan)
		pg.section("Software", s.Software)
	}
	pg.off += 14
	pg.text(false, 8, "Generated demo content for testing the "+conference+" website.")
	return pg.pdf(s.Title, conference+" demo data")
}

// pdfPage lays out lines of text from the top of one A4 page.
type pdfPage struct {
	off     float64 // distance from the top margin to the last baseline
	content bytes.Buffer
}

const (
	pageTop    = 790.0
	pageBottom = 56.0
	pageLeft   = 64.0
)

func (pg *pdfPage) text(bold bool, size float64, s string) {
	pg.off += size * 1.4
	y := pageTop - pg.off
	if y < pageBottom {
		return // one page is enough for demo data
	}
	font := "F2"
	if bold {
		font = "F1"
	}
	fmt.Fprintf(&pg.content, "BT /%s %g Tf %g %.1f Td (%s) Tj ET\n", font, size, pageLeft, y, pdfEscape(s))
}

func (pg *pdfPage) section(heading, body string) {
	pg.off += 10
	pg.text(true, 11, heading)
	for _, para := range strings.Split(body, "\n") {
		for _, line := range wrap(para, 92) {
			pg.text(false, 10, line)
		}
	}
}

// pdf assembles a PDF 1.4 file around the page using the standard
// Helvetica fonts, which need no embedding.
func (pg *pdfPage) pdf(title, producer string) []byte {
	stream := pg.content.String()
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 4 0 R /F2 5 0 R >> >> /Contents 6 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		fmt.Sprintf("<< /Title (%s) /Producer (%s) >>", pdfEscape(title), pdfEscape(producer)),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info 7 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

// pdfEscape makes s safe inside a PDF string. The standard fonts cover
// Latin text only, so other characters get an ASCII stand-in.
func pdfEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '–' || r == '—':
			b.WriteByte('-')
		case r == '‘' || r == '’':
			b.WriteByte('\'')
		case r == '“' || r == '”':
			b.WriteByte('"')
		case r < 0x20 || r > 0x7e:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wrap breaks s into lines of at most width characters at spaces.
func wrap(s string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) > width:
			lines = append(lines, line)
			line = word
		default:
			line += " " + word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// ---------------- the demo records ----------------

var (
	roles = []string{"a PhD candidate", "an assistant professor", "a research engineer",
		"a senior lecturer", "a postdoctoral researcher", "a robotics engineer"}
	experiences = []string{
		"Two conference talks and a tutorial at a regional robotics summer school (2025).",
		"None yet.",
		"Teaching assistant for an undergraduate robotics course for three years; one invited talk at an industry meetup.",
		"Co-organized workshops at a national robotics symposium in 2024 and 2025.",
	}
	ips        = []string{"203.0.113.24", "198.51.100.7", "192.0.2.145", "2001:db8::3a", "203.0.113.88", "198.51.100.212", "192.0.2.61", "2001:db8:4::17"}
	userAgents = []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.1 Safari/605.1.15",
		"Mozilla/5.0 (X11; Linux x86_64; rv:150.0) Gecko/20100101 Firefox/150.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36 Edg/154.0.0.0",
	}
)

const (
	laptopUbuntu = "x86-64 CPU with 4 cores, 16 GB RAM, 30 GB free disk, Ubuntu 22.04 (native or WSL2) and one free USB-A port."
	laptopAny    = "Any 4-core CPU, 8 GB RAM, 15 GB free disk; Windows 11, macOS 14 or Ubuntu 22.04 with a current Chrome or Edge."
	laptopGPU    = "16 GB RAM, 40 GB free disk, Ubuntu 22.04; an NVIDIA GPU with 6 GB of memory is recommended (a slower CPU fallback is provided)."
	laptopDocker = "4-core CPU, 16 GB RAM, 25 GB free disk and Docker Desktop, installed and tested before the conference."
)

// proposals are listed in the order they arrived; IDs are numbered per
// track in that order.
var proposals = []proposal{
	{
		track: content.TrackWithWorkshop,
		title: "Hands-on ROS 2 Navigation: From Simulation to a Real Mobile Robot",
		abstract: "Navigation stacks look simple in tutorials and fall apart on real floors. This paper reports on three years of teaching ROS 2 Nav2 " +
			"to newcomers and distils the failure modes we saw most often: poor odometry, mis-tuned costmaps and missing recovery behaviours. " +
			"We present a compact reference configuration and a step-by-step tuning procedure, validated on twelve robots in four buildings. " +
			"In the workshop, participants take a robot from a simulated world to a physical base and tune it until it completes a delivery route without intervention.",
		topics:   []string{"Autonomous Robotics", "Simulation, Digital Twins and Benchmarking"},
		fileName: "ros2-nav-workshop-paper.pdf",
		speaker:  "Mei-Ling Chen", email: "mlchen@harborview.example.edu", affiliation: "Harborview University of Technology", country: "Taiwan",
		plan:     [4]string{"launch the simulated robot and the navigation stack", "build a map with SLAM and inspect the costmaps", "tune the controller and the recovery behaviours on the real robots", "run the delivery route and compare the results"},
		software: "ROS 2 Humble, Nav2, Gazebo 11, Python 3.10", targetOS: "Ubuntu 22.04 (native or WSL2), or the provided Docker image",
		hardware: "10 small differential-drive robots with LiDAR (one per group of 2-3 participants) and spare batteries",
		skill:    "Intermediate", size: "25", laptop: laptopUbuntu, taCount: "2", taNames: "Wei-Ting Huang (Harborview University of Technology)\nJun Yamada (Harborview University of Technology)",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Runtime Monitoring of Learned Perception in Automated Driving",
		abstract: "Learned perception components fail silently when inputs drift away from the training data. We propose a runtime monitor that combines " +
			"feature-space novelty detection with temporal consistency checks across camera frames. On three public driving datasets with synthetic and " +
			"natural distribution shifts, the monitor detects 92 percent of perception failures at a false alarm rate of 2 percent, with 4 ms overhead per frame. " +
			"We discuss how such monitors can feed a safety case.",
		topics:   []string{"Safety, Assurance and Verification", "Perception and Situational Awareness", "Autonomous Vehicles and Mobility"},
		fileName: "runtime-monitor-perception.pdf",
		speaker:  "Priya Nair", email: "priya.nair@ida.example.org", affiliation: "Institute for Dependable Autonomy", country: "India",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "A Benchmark for Long-Horizon Planning in Partially Observable Warehouses",
		abstract: "Planning benchmarks for robotics often assume full observability or short horizons. We introduce a benchmark of 120 warehouse tasks " +
			"that require hundreds of decisions under partial observability, with a common interface for classical, learning-based and hybrid planners. " +
			"Baseline results show that hybrid planners solve 58 percent of the tasks, against 31 percent for the best purely learned planner. " +
			"The benchmark, the baselines and a leaderboard are openly available.",
		topics:   []string{"Planning, Decision-Making and Reasoning", "Simulation, Digital Twins and Benchmarking"},
		fileName: "lindqvist_planning_benchmark_draft.pdf",
		speaker:  "Jonas Lindqvist", email: "jonas.lindqvist@nordvik.example.org", affiliation: "Nordvik University", country: "Sweden",
		coSpeakers: "Elin Sandberg, Nordvik University, elin.sandberg@nordvik.example.org",
	},
	{
		track: content.TrackWithWorkshop,
		title: "Lane Following on an Embedded GPU: A 90-Minute Build",
		abstract: "Embedded GPUs make camera-based autonomy affordable, but the path from a trained network to a robust real-time controller is rarely taught. " +
			"We describe a minimal pipeline for small vehicles: data collection with a joystick, a compact convolutional network, inference optimisation " +
			"and a PID steering controller. On a 1:10 scale car the pipeline runs at 45 frames per second and completes a 30 m track with no manual corrections. " +
			"Workshop participants collect their own data, train on their laptops and deploy to the car.",
		topics:   []string{"Edge and Embedded Computing for Autonomy", "Perception and Situational Awareness", "Autonomous Vehicles and Mobility"},
		fileName: "Becker_lane_following_ICAAS.pdf",
		speaker:  "Lukas Becker", email: "lukas.becker@rheinfeld.example.org", affiliation: "Rheinfeld Institute of Applied Robotics", country: "Germany",
		plan:     [4]string{"collect driving data with the joystick", "train the network on the laptop", "optimise the network and deploy it to the car", "tune the steering controller and race the track"},
		software: "Python 3.10, PyTorch 2, OpenCV 4", targetOS: "Windows 11, macOS 14 or Ubuntu 22.04",
		hardware: "10 1:10 scale cars with an embedded GPU board and a camera, and a taped track",
		skill:    "Beginner", size: "25", laptop: laptopGPU, taCount: "1", taNames: "Ahmad Faiz",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Calibrating Trust in Human-Robot Teams under Communication Delays",
		abstract: "When communication with a remote robot is delayed, operators either over-trust or abandon the autonomy. We report a study with 48 participants " +
			"supervising a remote inspection robot under delays of 0.5 to 4 seconds. Explaining the robot's intended next action restored appropriate reliance " +
			"at delays up to 2 seconds, while confidence displays alone did not. We derive design guidelines for supervisory interfaces.",
		topics:   []string{"Human–Autonomy Teaming and Interaction"},
		fileName: "trust-delays.pdf",
		speaker:  "Emily Carter", email: "ecarter@westbrook.example.edu", affiliation: "Westbrook College of Engineering", country: "United States",
	},
	{
		track: content.TrackWithWorkshop,
		title: "Formation Flight with Micro-Drones in Ninety Minutes",
		abstract: "Swarm robotics is usually taught in simulation because real swarms are expensive and fragile. We show that indoor micro-drones with an " +
			"external positioning system are a practical alternative for teaching. The paper presents a decentralised formation controller that runs on each drone, " +
			"a safety layer that keeps the drones inside a virtual cage, and lessons from eight teaching sessions. Participants program formations in Python, " +
			"test them in simulation and then fly them with up to twenty drones at once.",
		topics:   []string{"Multi-Agent and Swarm Autonomy", "Autonomous Robotics", "Control of Autonomous Systems"},
		fileName: "swarm-formation-icas2027.pdf",
		speaker:  "Aisha Rahman", email: "aisha.rahman@northgate.example.edu", affiliation: "Northgate Polytechnic", country: "Malaysia",
		coSpeakers: "Farid Hassan, Northgate Polytechnic, farid.hassan@northgate.example.edu",
		plan:       [4]string{"fly a single drone with the Python API", "implement the formation controller in simulation", "fly the formation with the real swarm inside the safety cage", "add a leader change and present the result"},
		software:   "Python 3.11, the drone vendor's Python library and the provided simulator", targetOS: "Windows 11, macOS 14 or Ubuntu 22.04",
		hardware: "20 micro-drones with spare propellers and batteries, a positioning system and a netted flight area",
		skill:    "Intermediate", size: "50", laptop: laptopAny, taCount: "3 or more", taNames: "Nurul Izzah\nKevin Tan\nSiti Aminah",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Energy-Aware Task Allocation for Heterogeneous Drone Swarms",
		abstract: "Battery limits dominate the performance of drone swarms in long missions. We formulate task allocation for drones with different payloads " +
			"and batteries as a mixed-integer problem and propose a market-based heuristic that runs onboard. In simulated search-and-rescue missions with up to " +
			"60 drones, the heuristic comes within 7 percent of the optimal mission time while halving forced landings compared with energy-agnostic allocation.",
		topics:   []string{"Multi-Agent and Swarm Autonomy", "Planning, Decision-Making and Reasoning"},
		fileName: "energy_aware_allocation.pdf",
		speaker:  "Yusuf Demir", email: "yusuf.demir@anatolia-tech.example.org", affiliation: "Anatolia Technical University", country: "Türkiye",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Reachability Analysis for Neural Network Controllers with Tight Bounds",
		abstract: "Neural network controllers are hard to verify because reachable sets grow quickly over time. We present a reachability method that splits " +
			"the input set adaptively and reuses bounds across time steps. On six standard benchmarks the method proves safety for horizons up to three times " +
			"longer than existing tools within the same time budget. We release the tool and the benchmark configurations.",
		topics:   []string{"Safety, Assurance and Verification", "Control of Autonomous Systems"},
		fileName: "reachability-nn-controllers.pdf",
		speaker:  "Chloé Martin", email: "chloe.martin@essa.example.org", affiliation: "École Supérieure des Systèmes Autonomes", country: "France",
	},
	{
		track: content.TrackWithWorkshop,
		title: "A Digital Twin for Warehouse Robot Fleets with Open-Source Tools",
		abstract: "Fleet operators need to test new traffic rules and layouts before they touch the real warehouse. We present an open-source digital twin " +
			"that couples a physics simulator with the fleet manager used in production, so the same software runs against virtual and real robots. " +
			"In a case study with 40 robots, the twin predicted throughput within 6 percent of the measured value and revealed two deadlocks before deployment. " +
			"In the workshop, participants build a small warehouse, connect a fleet manager and find the bottleneck in a given layout.",
		topics:   []string{"Simulation, Digital Twins and Benchmarking", "Applications of Autonomous Systems", "Planning, Decision-Making and Reasoning"},
		fileName: "warehouse-digital-twin.pdf",
		speaker:  "Daniel Okafor", email: "d.okafor@lagooncity.example.edu", affiliation: "Lagoon City University", country: "Nigeria",
		plan:     [4]string{"start the twin and the fleet manager", "model a warehouse layout and add robots", "run the order scenario and measure throughput", "find and fix the bottleneck"},
		software: "Docker Desktop with the provided containers (simulator, fleet manager, dashboard)", targetOS: "Windows 11, macOS 14 or Ubuntu 22.04 with Docker",
		hardware: "None (laptop only)",
		skill:    "Intermediate", size: "50", laptop: laptopDocker, taCount: "2", taNames: "Chinedu Eze\nGrace Adebayo",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Self-Adaptive Software Architectures for Long-Lived Autonomous Systems",
		abstract: "Autonomous systems deployed for years must adapt to new missions, hardware and regulations without being rebuilt. We present an architecture " +
			"that separates mission logic from adaptation logic and uses models at runtime to reason about reconfiguration. Applied to an underwater inspection " +
			"vehicle over 14 months, the architecture handled 23 changes in mission or hardware without redeploying the core software.",
		topics:   []string{"Autonomic Computing and Self-* Systems", "Foundations and Architectures of Autonomous Systems"},
		fileName: "self-adaptive-architecture.pdf",
		speaker:  "Ravi Shankar", email: "ravi.shankar@deccan-tech.example.org", affiliation: "Deccan Institute of Technology", country: "India",
	},
	{
		track: content.TrackWithWorkshop,
		title: "Safe Reinforcement Learning for Robot Arms with Runtime Shielding",
		abstract: "Reinforcement learning can teach a robot arm new skills, but exploration on hardware is risky. We combine a learned policy with a runtime " +
			"shield that projects unsafe actions onto a verified safe set. On a pick-and-place task the shield prevented all 214 collisions observed without it, " +
			"while the success rate dropped by only 3 percent. The workshop walks through training a policy in simulation, synthesising a shield and " +
			"transferring both to a desktop robot arm.",
		topics:   []string{"Learning for Autonomy", "Safety, Assurance and Verification", "Autonomous Robotics"},
		fileName: "shielded_rl_arms_v2.pdf",
		speaker:  "Sofia Rossi", email: "sofia.rossi@valmonte.example.org", affiliation: "Politecnico di Valmonte", country: "Italy",
		plan:     [4]string{"train a baseline policy in simulation", "observe unsafe actions and define the safe set", "add the shield and retrain", "transfer the policy and the shield to the desktop arms"},
		software: "Python 3.10, PyTorch, MuJoCo 3 and the provided shield library", targetOS: "Ubuntu 22.04 or macOS 14 (Windows through WSL2)",
		hardware: "10 desktop robot arms with grippers and test objects",
		skill:    "Advanced", size: "25", laptop: laptopGPU, taCount: "1", taNames: "Marco Bianchi",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Physical Adversarial Patches against LiDAR Object Detection",
		abstract: "Most work on adversarial robustness targets cameras, yet LiDAR is widely trusted as the safer sensor. We show that small reflective patches " +
			"on roadside objects can make three popular LiDAR detectors miss pedestrians at distances up to 18 m. We analyse why the attack works and evaluate " +
			"two defences: point-cloud consistency checks and multi-sensor cross-validation, the latter reducing the attack success rate from 71 to 9 percent.",
		topics:   []string{"Security, Privacy and Resilience", "Perception and Situational Awareness"},
		fileName: "lidar-adversarial-patches.pdf",
		speaker:  "Ji-woo Park", email: "jiwoo.park@hanbit.example.org", affiliation: "Hanbit National University", country: "South Korea",
	},
	{
		track: content.TrackWithWorkshop,
		title: "Sensor Fusion from Scratch: An Extended Kalman Filter for a Small Ground Robot",
		abstract: "Many robotics students use sensor fusion packages without understanding them, which turns tuning into guesswork. We present a teaching " +
			"sequence that builds an extended Kalman filter step by step, fusing wheel odometry, an IMU and occasional GNSS fixes, and visualises every step " +
			"so that the effect of each noise parameter can be seen. An evaluation with 64 students showed a clear improvement in their ability to diagnose drift. " +
			"Participants implement the filter in Python and run it on logged and live data from a small ground robot.",
		topics:   []string{"Perception and Situational Awareness", "Control of Autonomous Systems"},
		fileName: "ekf-from-scratch.pdf",
		speaker:  "Kenji Watanabe", email: "kenji.watanabe@shirakawa.example.org", affiliation: "Shirakawa Institute of Science", country: "Japan",
		plan:     [4]string{"load the logged data and plot the raw sensors", "implement the prediction step with the odometry model", "add the IMU and GNSS updates", "run the filter live on the robot"},
		software: "Python 3.11, NumPy, Matplotlib and Jupyter", targetOS: "Any system that runs Python 3.11",
		hardware: "10 small ground robots with an IMU and a GNSS receiver, and recorded datasets on USB drives",
		skill:    "Beginner", size: "25", laptop: laptopAny, taCount: "1",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "What Pedestrians Expect from Delivery Robots: A Field Study",
		abstract: "Sidewalk delivery robots share space with people who never agreed to take part in an experiment. We observed 1,900 encounters between " +
			"pedestrians and delivery robots in two cities and interviewed 85 pedestrians. People expected robots to yield, to signal their intent and to avoid " +
			"blocking narrow passages; violations were judged more harshly than the same behaviour by humans. We discuss implications for regulation and robot design.",
		topics:   []string{"Ethics, Policy and Societal Impact", "Applications of Autonomous Systems"},
		fileName: "delivery-robots-field-study.pdf",
		speaker:  "Hannah Müller", email: "hannah.mueller@alpenstadt.example.org", affiliation: "Alpenstadt University", country: "Austria",
	},
	{
		track: content.TrackWithWorkshop,
		title: "Model Predictive Control for Autonomous Racing on a Budget",
		abstract: "Autonomous racing is an engaging way to teach optimal control, yet most courses stop at simulation. We present an MPC formulation for " +
			"1:10 scale race cars that runs at 50 Hz on a single-board computer, together with a system identification routine that participants can complete " +
			"in fifteen minutes. On our test track the controller beats a tuned pure-pursuit baseline by 18 percent in lap time. In the workshop, teams " +
			"identify their car, tune the controller and compete in a time trial.",
		topics:   []string{"Control of Autonomous Systems", "Autonomous Vehicles and Mobility"},
		fileName: "MPC racing - ICAAS.pdf",
		speaker:  "Camila Duarte", email: "camila.duarte@litoralsur.example.org", affiliation: "Universidad del Litoral Sur", country: "Chile",
		coSpeakers: "Matías Herrera, Universidad del Litoral Sur, matias.herrera@litoralsur.example.org",
		plan:       [4]string{"identify the vehicle model from logged runs", "formulate and solve the MPC problem", "tune the horizon and the weights on the track", "run the time trial and debrief"},
		software:   "Python 3.10, CasADi, acados and ROS 2 Humble", targetOS: "Ubuntu 22.04 (native or WSL2)",
		hardware: "20 1:10 scale race cars with single-board computers, a timing gate and a track",
		skill:    "Advanced", size: "50", laptop: laptopUbuntu, taCount: "3 or more", taNames: "Ignacio Vera\nPaula Soto\nDiego Fuentes",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Few-Shot Imitation Learning for Mobile Manipulation in Homes",
		abstract: "Household robots must learn new tasks from very few demonstrations. We present a method that combines a pretrained visual representation " +
			"with a skill library and learns new mobile manipulation tasks from five demonstrations. Across 18 tasks in three real apartments, the method " +
			"succeeds in 64 percent of the trials, compared with 22 percent for behaviour cloning trained from scratch.",
		topics:   []string{"Learning for Autonomy", "Autonomous Robotics"},
		fileName: "few-shot-mobile-manipulation.pdf",
		speaker:  "Tomás García", email: "tomas.garcia@altamar.example.org", affiliation: "Universidad de Altamar", country: "Spain",
	},
	{
		track: content.TrackWithWorkshop,
		title: "From Teleoperation to Autonomy: Shared Control for Field Robots",
		abstract: "Field robots are rarely fully autonomous; operators take over when the robot is unsure. We study shared control interfaces that blend " +
			"operator commands with autonomous behaviour and present a design that adapts the blending to the robot's own uncertainty estimate. In a user " +
			"study with 24 operators, task completion time fell by 27 percent and interventions by 41 percent compared with pure teleoperation. " +
			"Participants build a shared-control interface for a simulated rover and test it on a real one.",
		topics:   []string{"Human–Autonomy Teaming and Interaction", "Autonomous Robotics"},
		fileName: "shared-control.pdf",
		speaker:  "Hao Lin", email: "hao.lin@formosa-robotics.example.com", affiliation: "Formosa Robotics Lab", country: "Taiwan",
		plan:     [4]string{"drive the simulated rover by teleoperation", "add the autonomous behaviour and the blending", "adapt the blending to the robot's uncertainty", "test the interface on the real rover"},
		software: "Python 3.11, ROS 2 Humble and a web browser", targetOS: "Ubuntu 22.04 or the provided virtual machine",
		hardware: "10 tracked rovers with cameras and controllers, and a rough-terrain course",
		skill:    "Intermediate", size: "25", laptop: laptopUbuntu, taCount: "2", taNames: "Yu-Chen Lai\nPo-Han Wu",
	},
	{
		track: content.TrackWithoutWorkshop,
		title: "Map-Free Navigation in GNSS-Denied Underground Mines",
		abstract: "Underground mines change daily, so prebuilt maps are quickly outdated, and satellite positioning is unavailable. We present a navigation " +
			"approach for haul vehicles that follows the tunnel structure using LiDAR and topological cues instead of a metric map. Over 230 km of autonomous " +
			"driving in an operating mine, the vehicle needed one intervention every 46 km on average.",
		topics:     []string{"Autonomous Vehicles and Mobility", "Applications of Autonomous Systems", "Other"},
		otherTopic: "Mining automation",
		fileName:   "underground-navigation.pdf",
		speaker:    "Olivia Thompson", email: "olivia.thompson@redearth.example.org", affiliation: "Red Earth Institute of Technology", country: "Australia",
	},
}

var contacts = []server.ContactMessage{
	{Name: "Lena Hoffmann", Email: "lena.hoffmann@example.org", Topic: "General inquiry", Subject: "Remote presentation",
		Message: "Hello, if my paper is accepted, would it be possible to present remotely? I may not be able to travel because of a teaching commitment.\n\nBest regards,\nLena"},
	{Name: "Marcus Lee", Email: "marcus.lee@example.com", Topic: "Registration", Subject: "Student discount",
		Message: "Is there a reduced registration fee for PhD students, and which document do I need to show? Thanks!"},
	{Name: "Fatima Zahra", Email: "fatima.zahra@example.edu", Topic: "Visa invitation letter", Subject: "Invitation letter for my visa",
		Message: "I am an author of a submitted paper and will need an invitation letter for my visa application. What information do you need from me, and how long does it take?"},
	{Name: "Oliver Grant", Email: "o.grant@example.com", Topic: "Sponsorship & exhibition", Subject: "Exhibition booth",
		Message: "Our company builds robot kits for education. Could you send us the sizes and prices of the exhibition booths, and tell us whether we can run a short demo at the booth?"},
	{Name: "Lukas Becker", Email: "lukas.becker@rheinfeld.example.org", Topic: "Paper submission", Subject: "Updated PDF for {W002}",
		Message: "We found a typo in a figure caption after submitting. Can we replace the PDF of submission {W002}, or should we wait for the final version?"},
	{Name: "Sara Lindgren", Email: "sara.lindgren@example.org", Topic: "Media & press", Subject: "Press accreditation",
		Message: "I write for a technology magazine and would like to cover the workshops. How can I apply for press accreditation?"},
}

var subscribers = []server.Subscriber{
	{FirstName: "Anna", LastName: "Kowalska", Email: "anna.kowalska@example.edu", Organization: "Westvale University", Country: "Poland",
		WorkField: "Academia", Interests: []string{"Call for papers & deadlines", "Program & speakers"}},
	{FirstName: "Bruno", LastName: "Silva", Email: "bruno.silva@example.com", Organization: "Autonomia Systems", Country: "Brazil",
		WorkField: "Industry", Interests: []string{"Registration", "Program & speakers"}},
	{FirstName: "Yu-Hsuan", LastName: "Chen", Email: "yuhsuan.chen@example.edu", Organization: "Harborview University of Technology", Country: "Taiwan",
		WorkField: "Academia", Interests: []string{"Call for papers & deadlines"}},
	{FirstName: "David", LastName: "Cohen", Email: "david.cohen@example.org", Organization: "Institute for Mobility Research", Country: "Israel",
		WorkField: "Research institute", Interests: []string{"Program & speakers"}},
	{FirstName: "Eva", LastName: "Novak", Email: "eva.novak@example.com", Country: "Czechia",
		WorkField: "Other", Interests: []string{"Registration"}},
	{FirstName: "Farah", LastName: "Haddad", Email: "farah.haddad@example.org", Organization: "City Transport Office", Country: "Jordan",
		WorkField: "Government / public sector", Interests: []string{"Program & speakers", "Registration"}},
	{FirstName: "George", LastName: "Papadopoulos", Email: "g.papadopoulos@example.edu", Organization: "Aegean Technical University", Country: "Greece",
		WorkField: "Academia", Interests: []string{"Call for papers & deadlines", "Registration"}},
	{FirstName: "Hiroshi", LastName: "Sato", Email: "hiroshi.sato@example.com", Organization: "Kanto Mobility Inc.", Country: "Japan",
		WorkField: "Industry", Interests: []string{"Sponsorship & exhibition"}},
	{FirstName: "Isabel", LastName: "Ortiz", Email: "isabel.ortiz@example.org", Organization: "Robotics Testing Center", Country: "Mexico",
		WorkField: "Research institute", Interests: []string{"Program & speakers", "Sponsorship & exhibition"}},
	{FirstName: "Jakob", LastName: "Weber", Email: "jakob.weber@example.com", Organization: "AutoDrive Components", Country: "Germany",
		WorkField: "Industry", Interests: []string{"Call for papers & deadlines", "Registration", "Program & speakers", "Sponsorship & exhibition"}},
	{FirstName: "Kavya", LastName: "Reddy", Email: "kavya.reddy@example.edu", Organization: "Deccan Institute of Technology", Country: "India",
		WorkField: "Academia", Interests: []string{"Call for papers & deadlines"}},
	{FirstName: "Liam", LastName: "O'Brien", Email: "liam.obrien@example.org", Organization: "Harbour Authority", Country: "Ireland",
		WorkField: "Government / public sector", Interests: []string{"Program & speakers"}},
}
