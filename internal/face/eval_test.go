// W1 evaluation harness for the face-recognition plan.
//
// TestEvalMetrics covers the pure measurement code (IoU, detection matching
// and pairwise grouping metrics) and always runs. TestFaceEval is the gated
// end-to-end measurement: it renders each labelled photo to a thumbnail at a
// configurable size, runs the real detector daemon on it, matches detections
// back to the hand labels and reports recall and pairwise clustering quality.
// It is opt-in because it needs the pv-face-detect helper and an image corpus:
//
//	PV_FACE_EVAL_ROOT=/path/to/corpus \
//	PV_FACE_EVAL_SIZE=256 PV_FACE_EVAL_WRITE=1 \
//	go test ./internal/face -run TestFaceEval -v
//
// The corpus root must contain labels.json:
//
//	{"photos": [{"path": "a.jpg", "faces": [{"person": "ana", "bbox": [x,y,w,h]}]}]}
//
// with bbox normalised to 0..1 of the photo. The test only fails on I/O
// errors; the metrics themselves are a measurement, never a pass/fail gate.
package face

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/thumb"
)

// evalBox is a face box normalised to the unit square: x, y, width, height.
type evalBox [4]float64

// iou returns the intersection-over-union of two normalised boxes. It is 0
// when either box has no area or they do not overlap.
func iou(a, b evalBox) float64 {
	if a[2] <= 0 || a[3] <= 0 || b[2] <= 0 || b[3] <= 0 {
		return 0
	}
	iw := math.Min(a[0]+a[2], b[0]+b[2]) - math.Max(a[0], b[0])
	ih := math.Min(a[1]+a[3], b[1]+b[3]) - math.Max(a[1], b[1])
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := iw * ih
	union := a[2]*a[3] + b[2]*b[3] - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

// evalFace is one hand-labelled face.
type evalFace struct {
	Person string
	Box    evalBox
}

// evalDet is one detector output: its box, embedding and helper quality.
type evalDet struct {
	Box       evalBox
	Embedding []float32
	Quality   float64
}

// evalMetrics is the result of one evaluation run.
type evalMetrics struct {
	LabelledFaces, DetectedFaces, MatchedFaces int
	Recall, PairPrecision, PairRecall          float64
	Clusters, ImpureClusters                   int
}

// matchDetections greedily pairs detections with hand labels by descending
// IoU, requiring IoU >= 0.5. match[i] is the label index for detection i, or
// -1 when it was not matched. Each label is matched at most once.
func matchDetections(labels []evalFace, dets []evalDet) []int {
	type pair struct {
		det, label int
		score      float64
	}
	var pairs []pair
	for d := range dets {
		for l := range labels {
			if s := iou(dets[d].Box, labels[l].Box); s >= 0.5 {
				pairs = append(pairs, pair{det: d, label: l, score: s})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].score != pairs[j].score {
			return pairs[i].score > pairs[j].score
		}
		if pairs[i].det != pairs[j].det {
			return pairs[i].det < pairs[j].det
		}
		return pairs[i].label < pairs[j].label
	})

	match := make([]int, len(dets))
	for i := range match {
		match[i] = -1
	}
	labelUsed := make([]bool, len(labels))
	for _, p := range pairs {
		if match[p.det] >= 0 || labelUsed[p.label] {
			continue
		}
		match[p.det] = p.label
		labelUsed[p.label] = true
	}
	return match
}

// groupingMetrics scores a clustering assignment pairwise. person[i] and
// cluster[i] are the true person and the assigned cluster of matched face i.
// A true positive is a pair with the same assigned cluster and the same
// person; precision divides by pairs sharing a cluster and recall by pairs
// sharing a person, with 0/0 defined as 1. clusters counts distinct cluster
// ids and impure counts clusters spanning more than one person. Mismatched
// slice lengths are scored over the shorter prefix.
func groupingMetrics(person []string, cluster []int) (precision, recall float64, clusters, impure int) {
	n := len(person)
	if len(cluster) < n {
		n = len(cluster)
	}

	clusterPersons := make(map[int]map[string]struct{})
	var sameClusterPairs, samePersonPairs, truePositives int
	for i := 0; i < n; i++ {
		if clusterPersons[cluster[i]] == nil {
			clusterPersons[cluster[i]] = make(map[string]struct{})
		}
		clusterPersons[cluster[i]][person[i]] = struct{}{}
		for j := i + 1; j < n; j++ {
			sameCluster := cluster[i] == cluster[j]
			samePerson := person[i] == person[j]
			if sameCluster {
				sameClusterPairs++
			}
			if samePerson {
				samePersonPairs++
			}
			if sameCluster && samePerson {
				truePositives++
			}
		}
	}

	for _, people := range clusterPersons {
		if len(people) > 1 {
			impure++
		}
	}

	precision = 1
	if sameClusterPairs > 0 {
		precision = float64(truePositives) / float64(sameClusterPairs)
	}
	recall = 1
	if samePersonPairs > 0 {
		recall = float64(truePositives) / float64(samePersonPairs)
	}
	return precision, recall, len(clusterPersons), impure
}

// streamCluster clusters embeddings online, in input order, using the
// package's production nearest-cluster and centroid-update functions. It
// returns the assigned cluster index for each input.
func streamCluster(embs [][]float32) []int {
	var clusters []cache.Cluster
	out := make([]int, len(embs))
	for i, v := range embs {
		idx := NearestCluster(v, clusters)
		if idx < 0 {
			clusters = append(clusters, cache.Cluster{
				ID:       int64(len(clusters)),
				Centroid: UpdatedCentroid(nil, v, 0),
				Count:    1,
			})
			idx = len(clusters) - 1
		} else {
			clusters[idx].Centroid = UpdatedCentroid(clusters[idx].Centroid, v, clusters[idx].Count)
			clusters[idx].Count++
		}
		out[i] = idx
	}
	return out
}

func TestEvalMetrics(t *testing.T) {
	t.Run("iou", func(t *testing.T) {
		cases := []struct {
			name string
			a, b evalBox
			want float64
		}{
			{"identical", evalBox{0.1, 0.2, 0.3, 0.4}, evalBox{0.1, 0.2, 0.3, 0.4}, 1},
			{"disjoint", evalBox{0, 0, 0.2, 0.2}, evalBox{0.5, 0.5, 0.2, 0.2}, 0},
			{"half overlap", evalBox{0, 0, 1, 1}, evalBox{0.5, 0, 0.5, 1}, 0.5},
			{"zero area", evalBox{0, 0, 0, 1}, evalBox{0, 0, 1, 1}, 0},
		}
		for _, tc := range cases {
			if got := iou(tc.a, tc.b); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("%s: iou = %v, want %v", tc.name, got, tc.want)
			}
		}
	})

	t.Run("matchDetections", func(t *testing.T) {
		cases := []struct {
			name   string
			labels []evalFace
			dets   []evalDet
			want   []int
		}{
			{
				// Two detections both overlap one label well; only the best
				// may claim it and the other stays unmatched.
				name:   "label matched once",
				labels: []evalFace{{Person: "ana", Box: evalBox{0, 0, 1, 1}}},
				dets: []evalDet{
					{Box: evalBox{0, 0, 1, 1}},
					{Box: evalBox{0.02, 0, 0.98, 1}},
				},
				want: []int{0, -1},
			},
			{
				name:   "below threshold",
				labels: []evalFace{{Person: "ana", Box: evalBox{0, 0, 1, 1}}},
				dets:   []evalDet{{Box: evalBox{0.6, 0, 0.4, 1}}},
				want:   []int{-1},
			},
			{
				// Greedy must take the higher-IoU pair even when the lower-IoU
				// detection comes first in the slice.
				name:   "greedy by descending iou",
				labels: []evalFace{{Person: "ana", Box: evalBox{0, 0, 1, 1}}},
				dets: []evalDet{
					{Box: evalBox{0.2, 0, 0.8, 1}},
					{Box: evalBox{0.05, 0, 0.95, 1}},
				},
				want: []int{-1, 0},
			},
			{
				name: "distinct pairs",
				labels: []evalFace{
					{Person: "ana", Box: evalBox{0, 0, 0.5, 1}},
					{Person: "bob", Box: evalBox{0.5, 0, 0.5, 1}},
				},
				dets: []evalDet{
					{Box: evalBox{0.5, 0, 0.5, 1}},
					{Box: evalBox{0, 0, 0.5, 1}},
				},
				want: []int{1, 0},
			},
		}
		for _, tc := range cases {
			got := matchDetections(tc.labels, tc.dets)
			if len(got) != len(tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				continue
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
					break
				}
			}
		}
	})

	t.Run("groupingMetrics", func(t *testing.T) {
		cases := []struct {
			name                      string
			person                    []string
			cluster                   []int
			wantPrecision, wantRecall float64
			wantClusters, wantImpure  int
		}{
			{
				name:          "perfect grouping",
				person:        []string{"ana", "ana", "bob", "bob"},
				cluster:       []int{0, 0, 1, 1},
				wantPrecision: 1, wantRecall: 1,
				wantClusters: 2, wantImpure: 0,
			},
			{
				// Everyone in one cluster: recall stays 1 (each person's
				// faces stay together) but precision collapses.
				name:          "one mixed cluster",
				person:        []string{"ana", "ana", "bob", "bob"},
				cluster:       []int{0, 0, 0, 0},
				wantPrecision: 2.0 / 6.0, wantRecall: 1,
				wantClusters: 1, wantImpure: 1,
			},
			{
				// All singletons: no pairs in a common cluster (0/0 = 1 for
				// precision) but ana's two faces are split, so recall is 0.
				name:          "all singletons",
				person:        []string{"ana", "ana"},
				cluster:       []int{0, 1},
				wantPrecision: 1, wantRecall: 0,
				wantClusters: 2, wantImpure: 0,
			},
		}
		for _, tc := range cases {
			precision, recall, clusters, impure := groupingMetrics(tc.person, tc.cluster)
			if math.Abs(precision-tc.wantPrecision) > 1e-9 {
				t.Errorf("%s: precision = %v, want %v", tc.name, precision, tc.wantPrecision)
			}
			if math.Abs(recall-tc.wantRecall) > 1e-9 {
				t.Errorf("%s: recall = %v, want %v", tc.name, recall, tc.wantRecall)
			}
			if clusters != tc.wantClusters {
				t.Errorf("%s: clusters = %d, want %d", tc.name, clusters, tc.wantClusters)
			}
			if impure != tc.wantImpure {
				t.Errorf("%s: impure = %d, want %d", tc.name, impure, tc.wantImpure)
			}
		}
	})
}

// evalLabels is the on-disk labels.json schema.
type evalLabels struct {
	Photos []struct {
		Path  string `json:"path"`
		Faces []struct {
			Person string     `json:"person"`
			BBox   [4]float64 `json:"bbox"`
		} `json:"faces"`
	} `json:"photos"`
}

// evalResult is the JSON written next to the corpus when PV_FACE_EVAL_WRITE=1.
type evalResult struct {
	Size           int     `json:"size"`
	LabelledFaces  int     `json:"labelled_faces"`
	DetectedFaces  int     `json:"detected_faces"`
	MatchedFaces   int     `json:"matched_faces"`
	Recall         float64 `json:"recall"`
	PairPrecision  float64 `json:"pair_precision"`
	PairRecall     float64 `json:"pair_recall"`
	Clusters       int     `json:"clusters"`
	ImpureClusters int     `json:"impure_clusters"`
	// The same corpus grouped by the regroup pass instead of the streaming
	// assigner, so the two modes can be compared run to run.
	RegroupPairPrecision float64 `json:"regroup_pair_precision"`
	RegroupPairRecall    float64 `json:"regroup_pair_recall"`
	RegroupClusters      int     `json:"regroup_clusters"`
	RegroupImpure        int     `json:"regroup_impure"`
}

// TestFaceEval is the gated end-to-end evaluation. It is a measurement, not an
// assertion: only I/O problems fail it.
func TestFaceEval(t *testing.T) {
	root := os.Getenv("PV_FACE_EVAL_ROOT")
	if root == "" {
		t.Skip("PV_FACE_EVAL_ROOT not set; skipping gated face evaluation")
	}
	if !Available() {
		t.Skip("pv-face-detect not available; skipping gated face evaluation")
	}

	size := 256
	if s := os.Getenv("PV_FACE_EVAL_SIZE"); s != "" {
		n, err := fmt.Sscanf(s, "%d", &size)
		if err != nil || n != 1 || size <= 0 {
			t.Fatalf("PV_FACE_EVAL_SIZE=%q is not a positive integer", s)
		}
	}

	raw, err := os.ReadFile(filepath.Join(root, "labels.json"))
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	var labels evalLabels
	if err := json.Unmarshal(raw, &labels); err != nil {
		t.Fatalf("parse labels.json: %v", err)
	}

	var d *daemon
	if len(labels.Photos) > 0 {
		d, err = spawnDaemon()
		if err != nil {
			t.Fatalf("spawn %s: %v", BinaryName, err)
		}
		defer d.Close()
	}

	ctx := context.Background()
	renditionDir := t.TempDir()

	var (
		persons    []string
		embeddings [][]float32
		qualities  []float64
		labelled   int
		detected   int
		matched    int
	)

	for i, photo := range labels.Photos {
		src := filepath.Join(root, filepath.FromSlash(photo.Path))
		dst := filepath.Join(renditionDir, fmt.Sprintf("photo-%d.jpg", i))
		if err := thumb.Image(ctx, src, dst, size); err != nil {
			t.Fatalf("render %s: %v", src, err)
		}

		f, err := os.Open(dst)
		if err != nil {
			t.Fatalf("open rendition %s: %v", dst, err)
		}
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Fatalf("decode rendition config %s: %v", dst, err)
		}
		if cfg.Width <= 0 || cfg.Height <= 0 {
			t.Fatalf("rendition %s has non-positive size %dx%d", dst, cfg.Width, cfg.Height)
		}

		dets, err := d.Detect(ctx, dst)
		if err != nil {
			t.Fatalf("detect %s: %v", dst, err)
		}

		faces := make([]evalFace, len(photo.Faces))
		for j, face := range photo.Faces {
			faces[j] = evalFace{Person: face.Person, Box: evalBox(face.BBox)}
		}
		boxes := make([]evalDet, len(dets))
		for j, det := range dets {
			boxes[j] = evalDet{Box: normaliseDetBox(det.BBox, cfg.Width, cfg.Height), Embedding: det.Embedding, Quality: det.Quality}
		}

		match := matchDetections(faces, boxes)
		labelled += len(faces)
		detected += len(dets)
		for j, labelIdx := range match {
			if labelIdx < 0 {
				continue
			}
			matched++
			persons = append(persons, faces[labelIdx].Person)
			embeddings = append(embeddings, boxes[j].Embedding)
			qualities = append(qualities, boxes[j].Quality)
		}
	}

	cluster := streamCluster(embeddings)
	pairPrecision, pairRecall, clusters, impure := groupingMetrics(persons, cluster)

	// The same matched faces through the production regroup pass, so the
	// report shows whether re-clustering from the stored embeddings beats the
	// streaming assigner on this corpus.
	regroupFaces := make([]cache.RegroupFace, len(embeddings))
	for i, emb := range embeddings {
		regroupFaces[i] = cache.RegroupFace{ID: int64(i + 1), Embedding: emb, Quality: qualities[i]}
	}
	regrouped, err := regroupCore(context.Background(), regroupFaces, nil, RegroupOptions{})
	if err != nil {
		t.Fatalf("regroupCore: %v", err)
	}
	regroupClusterOf := make(map[int64]int, len(regroupFaces))
	for ci, c := range regrouped {
		for _, id := range c.Members {
			regroupClusterOf[id] = ci
		}
	}
	regroupCluster := make([]int, len(regroupFaces))
	for i, f := range regroupFaces {
		if ci, ok := regroupClusterOf[f.ID]; ok {
			regroupCluster[i] = ci
		} else {
			// Unclustered faces are singletons, not one shared "cluster".
			regroupCluster[i] = -1 - i
		}
	}
	regroupPrecision, regroupRecall, regroupClusters, regroupImpure := groupingMetrics(persons, regroupCluster)

	recall := 1.0
	if labelled > 0 {
		recall = float64(matched) / float64(labelled)
	}
	m := evalMetrics{
		LabelledFaces:  labelled,
		DetectedFaces:  detected,
		MatchedFaces:   matched,
		Recall:         recall,
		PairPrecision:  pairPrecision,
		PairRecall:     pairRecall,
		Clusters:       clusters,
		ImpureClusters: impure,
	}

	t.Logf("face eval: size=%d labelled=%d detected=%d matched=%d recall=%.3f pair_precision=%.3f pair_recall=%.3f clusters=%d impure=%d",
		size, m.LabelledFaces, m.DetectedFaces, m.MatchedFaces, m.Recall,
		m.PairPrecision, m.PairRecall, m.Clusters, m.ImpureClusters)
	t.Logf("face eval (regroup): pair_precision=%.3f pair_recall=%.3f clusters=%d impure=%d",
		regroupPrecision, regroupRecall, regroupClusters, regroupImpure)

	if os.Getenv("PV_FACE_EVAL_WRITE") == "1" {
		out := evalResult{
			Size:                 size,
			LabelledFaces:        m.LabelledFaces,
			DetectedFaces:        m.DetectedFaces,
			MatchedFaces:         m.MatchedFaces,
			Recall:               m.Recall,
			PairPrecision:        m.PairPrecision,
			PairRecall:           m.PairRecall,
			Clusters:             m.Clusters,
			ImpureClusters:       m.ImpureClusters,
			RegroupPairPrecision: regroupPrecision,
			RegroupPairRecall:    regroupRecall,
			RegroupClusters:      regroupClusters,
			RegroupImpure:        regroupImpure,
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			t.Fatalf("marshal eval result: %v", err)
		}
		path := filepath.Join(root, fmt.Sprintf("eval-result-%d.json", size))
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatalf("write eval result: %v", err)
		}
	}
}

// normaliseDetBox converts a pixel-space detector box into the 0..1 space of
// the rendition, clamping any part the detector placed outside the image.
func normaliseDetBox(b [4]int, width, height int) evalBox {
	x := clamp01(float64(b[0]) / float64(width))
	y := clamp01(float64(b[1]) / float64(height))
	w := math.Min(clamp01(float64(b[2])/float64(width)), 1-x)
	h := math.Min(clamp01(float64(b[3])/float64(height)), 1-y)
	return evalBox{x, y, w, h}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
