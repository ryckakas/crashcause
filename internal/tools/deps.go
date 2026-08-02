// Package tools has no runtime effect. It exists solely to blank-import
// modules that later crashcause milestones depend on but that no
// production code imports yet. Without a real import somewhere in the
// module, `go mod tidy` would prune these requirements from go.mod/go.sum,
// forcing whoever implements the next milestone to re-add them by hand.
//
// Each import below should be deleted, one at a time, once real code in
// the module imports the corresponding package for its own purposes. This
// file should shrink over time and eventually disappear entirely; it must
// never grow new imports for packages that are actually used elsewhere.
package tools

import (
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
	_ "golang.org/x/time/rate"
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/cli-runtime/pkg/genericclioptions"
	_ "k8s.io/client-go/informers"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/tools/clientcmd"
	_ "k8s.io/client-go/tools/leaderelection"
)
