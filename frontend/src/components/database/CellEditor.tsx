import { useEffect, useRef, useState } from "react";
import { Check } from "lucide-react";
import type { ColumnDef } from "~/api/database";

interface CellProps {
  col: ColumnDef;
  value: unknown;
  onChange: (next: unknown) => void;
}

// CellEditor dispatches on the column type. Each branch is its own
// small editor so the inline UI stays focused — no monster switch in
// the consumer. text/number/url use a click-to-edit input;
// select/multi_select render a dropdown / chips; checkbox is a
// toggle; date is a native date input; formula + relation are
// read-only stubs that surface the saved value.
export function CellEditor({ col, value, onChange }: CellProps) {
  switch (col.type) {
    case "checkbox":
      return <CheckboxCell value={!!value} onChange={onChange} />;
    case "select":
      return <SelectCell col={col} value={(value as string) ?? ""} onChange={onChange} />;
    case "multi_select":
      return <MultiSelectCell col={col} value={(value as string[]) ?? []} onChange={onChange} />;
    case "date":
      return <DateCell value={(value as string) ?? ""} onChange={onChange} />;
    case "number":
      return <TextCell value={fmt(value)} onChange={(v) => onChange(toNumber(v))} numeric />;
    case "url":
      return <UrlCell value={(value as string) ?? ""} onChange={onChange} />;
    case "relation":
    case "formula":
      return <span className="text-muted">{fmt(value) || "—"}</span>;
    default:
      return <TextCell value={fmt(value)} onChange={onChange} />;
  }
}

function fmt(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (Array.isArray(v)) return v.join(", ");
  return String(v);
}

function toNumber(v: unknown): number | string {
  const s = typeof v === "string" ? v : String(v ?? "");
  if (s === "") return "";
  const n = Number(s);
  return Number.isFinite(n) ? n : s;
}

// TextCell renders a click-to-edit input. The "view mode" is a plain
// span so the table reads cleanly; clicking promotes to a real input
// that commits on blur or Enter.
function TextCell({
  value,
  onChange,
  numeric,
}: {
  value: string;
  onChange: (next: unknown) => void;
  numeric?: boolean;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(value);
  const inputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => setDraft(value), [value]);
  useEffect(() => {
    if (editing) inputRef.current?.focus();
  }, [editing]);

  const commit = () => {
    setEditing(false);
    if (draft !== value) onChange(draft);
  };

  if (!editing) {
    return (
      <button
        onClick={() => setEditing(true)}
        className="w-full text-left text-xs text-text"
      >
        {value || <span className="text-muted">empty</span>}
      </button>
    );
  }
  return (
    <input
      ref={inputRef}
      type={numeric ? "number" : "text"}
      value={draft}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === "Enter") commit();
        else if (e.key === "Escape") setEditing(false);
      }}
      className="w-full rounded border border-accent bg-bg px-1 py-0.5 text-xs focus:outline-none"
    />
  );
}

function CheckboxCell({ value, onChange }: { value: boolean; onChange: (next: unknown) => void }) {
  return (
    <button
      onClick={() => onChange(!value)}
      className={`flex h-4 w-4 items-center justify-center rounded border ${
        value ? "border-accent bg-accent text-bg" : "border-border bg-bg"
      }`}
    >
      {value ? <Check size={10} /> : null}
    </button>
  );
}

function SelectCell({
  col,
  value,
  onChange,
}: {
  col: ColumnDef;
  value: string;
  onChange: (next: unknown) => void;
}) {
  return (
    <select
      value={value}
      onChange={(e) => onChange(e.target.value)}
      className="w-full rounded border border-border bg-bg px-1 py-0.5 text-xs"
    >
      <option value="">—</option>
      {(col.options ?? []).map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </select>
  );
}

function MultiSelectCell({
  col,
  value,
  onChange,
}: {
  col: ColumnDef;
  value: string[];
  onChange: (next: unknown) => void;
}) {
  const toggle = (o: string) => {
    onChange(value.includes(o) ? value.filter((v) => v !== o) : [...value, o]);
  };
  return (
    <div className="flex flex-wrap gap-1">
      {(col.options ?? []).map((o) => (
        <button
          key={o}
          onClick={() => toggle(o)}
          className={`rounded border px-1 py-px text-[10px] ${
            value.includes(o)
              ? "border-accent bg-accent/15 text-accent"
              : "border-border text-muted hover:border-accent"
          }`}
        >
          {o}
        </button>
      ))}
    </div>
  );
}

function DateCell({ value, onChange }: { value: string; onChange: (next: unknown) => void }) {
  return (
    <input
      type="date"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      className="w-full rounded border border-border bg-bg px-1 py-0.5 text-xs"
    />
  );
}

function UrlCell({ value, onChange }: { value: string; onChange: (next: unknown) => void }) {
  const [editing, setEditing] = useState(false);
  if (!editing) {
    return value ? (
      <button
        onClick={() => setEditing(true)}
        className="truncate text-xs text-accent underline"
      >
        {value}
      </button>
    ) : (
      <button onClick={() => setEditing(true)} className="text-xs text-muted">
        empty
      </button>
    );
  }
  return (
    <input
      autoFocus
      type="url"
      defaultValue={value}
      onBlur={(e) => {
        setEditing(false);
        if (e.target.value !== value) onChange(e.target.value);
      }}
      className="w-full rounded border border-accent bg-bg px-1 py-0.5 text-xs focus:outline-none"
    />
  );
}
