/* global React */
const { useState } = React;

/* Console-tag pill — geometry comes from colors_and_type.css .pill */
function Pill({ kind = "free", children }) {
  return <span className={`pill ${kind}`}>{children}</span>;
}

/* Neutral count chip — geometry comes from colors_and_type.css .count-pill */
function CountPill({ children }) {
  return <span className="count-pill">{children}</span>;
}

function LiveDot() { return <span className="live-dot" />; }

function Empty({ children }) { return <div className="empty">{children}</div>; }

/* Button — console plate (.gb). Kinds:
   primary  — green leading rail, affirmative action
   default  — neutral plate
   danger   — red leading rail
   quiet    — transparent plate, used for inline actions in chrome
   ghost    — strips the plate; plain text link (for table-cell actions)
*/
function Button({ kind = "primary", sigil, children, ...rest }) {
  const cls = ["gb", kind].filter(Boolean).join(" ");
  return (
    <button className={cls} {...rest}>
      {sigil ? <span className="sigil">{sigil}</span> : null}
      <span className="label">{children}</span>
    </button>
  );
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
