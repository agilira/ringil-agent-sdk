import { useId } from "react";
import { useI18n } from "./i18n";
interface GraphStep {
  name: string;
  worker: string;
  needs?: string[];
}
export function WorkflowGraph({ steps }: { steps: GraphStep[] }) {
  const { t } = useI18n();
  const marker = useId().replaceAll(":", "");
  const depth = new Map<string, number>();
  function level(name: string, seen = new Set<string>()): number {
    if (depth.has(name)) return depth.get(name)!;
    if (seen.has(name)) return 0;
    seen.add(name);
    const step = steps.find((s) => s.name === name);
    const value = step?.needs?.length
      ? 1 + Math.max(...step.needs.map((n) => level(n, new Set(seen))))
      : 0;
    depth.set(name, value);
    return value;
  }
  for (const step of steps) level(step.name);
  const counts = new Map<number, number>();
  const points = new Map(
    steps.map((s) => {
      const x = depth.get(s.name) ?? 0;
      const y = counts.get(x) ?? 0;
      counts.set(x, y + 1);
      return [s.name, { x: 20 + x * 230, y: 20 + y * 110 }];
    }),
  );
  const width = Math.max(1, ...depth.values()) * 230 + 230;
  const height = Math.max(1, ...counts.values()) * 110;
  return (
    <div className="workflow-graph">
      <svg
        role="img"
        aria-label={steps
          .map(
            (s) =>
              `${s.name}: ${s.worker}, ${t("needs")}: ${(s.needs ?? []).join(", ")}`,
          )
          .join("; ")}
        width={width}
        height={height}
        viewBox={`0 0 ${width} ${height}`}
      >
        <defs>
          <marker
            id={marker}
            viewBox="0 0 10 10"
            refX="9"
            refY="5"
            markerWidth="6"
            markerHeight="6"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--muted)" />
          </marker>
        </defs>
        {steps.flatMap((s) =>
          (s.needs ?? []).map((n) => {
            const a = points.get(n),
              b = points.get(s.name);
            if (!a || !b) return null;
            return (
              <path
                key={n + "-" + s.name}
                d={`M ${a.x + 180} ${a.y + 34} C ${a.x + 205} ${a.y + 34}, ${b.x - 25} ${b.y + 34}, ${b.x} ${b.y + 34}`}
                fill="none"
                stroke="var(--muted)"
                strokeWidth="2"
                markerEnd={`url(#${marker})`}
              />
            );
          }),
        )}
        {steps.map((s) => {
          const p = points.get(s.name)!;
          return (
            <g key={s.name} transform={`translate(${p.x},${p.y})`}>
              <rect
                width="180"
                height="70"
                rx="10"
                fill="var(--accent-soft)"
                stroke="var(--line)"
              />
              <text x="12" y="27" fill="var(--text)" fontSize="14">
                {s.name.length > 22 ? s.name.slice(0, 20) + "…" : s.name}
              </text>
              <text x="12" y="51" fill="var(--muted)" fontSize="14">
                {s.worker.length > 22 ? s.worker.slice(0, 20) + "…" : s.worker}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}
