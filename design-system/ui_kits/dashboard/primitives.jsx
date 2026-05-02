/* global React */
const { useState } = React;

function Pill({ kind = "free", children }) {
  return <span className={`pill ${kind}`}>{children}</span>;
}

function CountPill({ children }) {
  return <span className="count">{children}</span>;
}

function LiveDot() { return <span className="live-dot" />; }

function Empty({ children }) { return <div className="empty">{children}</div>; }

function Button({ kind = "primary", children, ...rest }) {
  const cls = kind === "ghost" ? "btn ghost" : kind === "link" ? "link" : kind === "danger" ? "link danger" : "btn";
  return <button className={cls} {...rest}>{children}</button>;
}

function EyebrowKey({ children }) {
  return <span style={{ color: "var(--fg-dim)", fontSize: "0.75rem", textTransform: "uppercase", letterSpacing: "0.04em", marginRight: 6 }}>{children}</span>;
}

function MonoCell({ children, dim }) {
  return <td className={`mono ${dim ? "dim" : ""}`}>{children}</td>;
}

function Field({ label, children }) {
  return (
    <label className="field">
      <span className="label">{label}</span>
      {children}
    </label>
  );
}

function relTime(iso) {
  if (!iso) return "never";
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 0) return "just now";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

Object.assign(window, { Pill, CountPill, LiveDot, Empty, Button, EyebrowKey, MonoCell, Field, relTime });
