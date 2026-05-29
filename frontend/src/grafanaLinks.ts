/**
 * Grafana Explore deep-link helpers.
 *
 * Why: when a native phase Job runs, its container stdout and the kube
 * events for the pod are shipped to Loki cluster-wide. The run-report UI
 * has historically had no affordance saying so — an operator (or agent)
 * staring at a stuck step had to discover the data was in Loki and
 * construct the LogQL by hand. This module produces a deep-link the UI
 * can render right next to the k8s job name so the discovery path is
 * one click.
 *
 * The link target is Grafana's Explore view with the Loki datasource and
 * a {namespace="...", pod=~"<job>-.*"} expression pre-filled. Grafana
 * accepts a JSON-encoded `left` query param documented at
 * https://grafana.com/docs/grafana/latest/explore/#share-explore.
 */
import type { GlimmungConfig } from "./auth";

export type LokiLinkRange = {
  /**
   * Inclusive start of the time window. Accepts either a Date / epoch-ms
   * number, or a Grafana relative string like "now-6h". Callers should
   * pass the job's started_at when known; the default is "now-24h".
   */
  from?: Date | number | string;
  /**
   * Exclusive end of the time window. Default is "now". Pass the job's
   * completed_at for terminal jobs so the link does not keep widening.
   */
  to?: Date | number | string;
};

/**
 * Build an Explore URL for one pod. Returns null when the cluster
 * Grafana base URL or the native-runner namespace is not configured —
 * the caller should suppress the affordance entirely in that case
 * rather than render a broken link.
 *
 * pod is matched as `pod=~"<pod>-.*"` to cover both the job name
 * (Glimmung's K8sJobName) and the kubelet-generated pod suffix.
 */
export function lokiExploreUrl(
  config: GlimmungConfig | null,
  k8sJobName: string | null | undefined,
  range: LokiLinkRange = {},
): string | null {
  if (!config) return null;
  const base = (config.grafana_base_url ?? "").replace(/\/+$/, "");
  const namespace = (config.native_runner_namespace ?? "").trim();
  const datasource = (config.grafana_loki_datasource ?? "loki").trim();
  const pod = (k8sJobName ?? "").trim();
  if (!base || !namespace || !pod) return null;

  const expr = `{namespace="${namespace}",pod=~"${escapeLogQL(pod)}-.*"}`;
  const left = {
    datasource,
    queries: [
      {
        refId: "A",
        datasource: { type: "loki", uid: datasource },
        expr,
        queryType: "range",
      },
    ],
    range: {
      from: encodeRangeBound(range.from, "now-24h"),
      to: encodeRangeBound(range.to, "now"),
    },
  };
  const params = new URLSearchParams({
    orgId: "1",
    left: JSON.stringify(left),
  });
  return `${base}/explore?${params.toString()}`;
}

/**
 * encodeRangeBound normalises a Date / number / string into Grafana's
 * Explore range encoding: epoch milliseconds as a string for absolute
 * bounds, the original string for "now"-relative bounds, and the
 * fallback for null/undefined.
 */
function encodeRangeBound(
  value: Date | number | string | undefined,
  fallback: string,
): string {
  if (value === undefined || value === null) return fallback;
  if (value instanceof Date) return String(value.getTime());
  if (typeof value === "number") return String(value);
  return value;
}

/**
 * escapeLogQL escapes the regex-meaningful characters that can show up
 * in a kubernetes job name. K8s job names are validated as DNS labels
 * (a-z, 0-9, '-') so in practice the only character we have to worry
 * about is '-', but the escape keeps the helper safe if that constraint
 * ever changes upstream.
 */
function escapeLogQL(value: string): string {
  return value.replace(/[\\.+*?()|[\]{}^$]/g, (m) => `\\${m}`);
}
