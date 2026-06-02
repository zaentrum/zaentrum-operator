import { Check } from 'lucide-react';

export interface Step {
  id: string;
  title: string;
}

/**
 * Horizontal wizard stepper. Completed steps show a check, the current step is
 * cloud-blue, future steps are dimmed. Used by the SetupWizard.
 */
export function Stepper({ steps, current }: { steps: Step[]; current: number }) {
  return (
    <ol className="flex items-center gap-s-2">
      {steps.map((step, i) => {
        const done = i < current;
        const active = i === current;
        return (
          <li key={step.id} className="flex flex-1 items-center gap-s-2">
            <span
              className={[
                'flex h-7 w-7 shrink-0 items-center justify-center rounded-full border text-xs font-semibold',
                done
                  ? 'border-signal-green bg-signal-green/15 text-signal-green'
                  : active
                    ? 'border-cloud-blue bg-cloud-blue/15 text-cloud-blue'
                    : 'border-border-2 bg-surface text-fg-dim',
              ].join(' ')}
            >
              {done ? <Check size={14} /> : i + 1}
            </span>
            <span
              className={[
                'whitespace-nowrap text-sm',
                active ? 'font-medium text-fg' : done ? 'text-fg-2' : 'text-fg-dim',
              ].join(' ')}
            >
              {step.title}
            </span>
            {i < steps.length - 1 ? (
              <span
                className={`mx-s-1 h-px flex-1 ${done ? 'bg-signal-green/40' : 'bg-border'}`}
              />
            ) : null}
          </li>
        );
      })}
    </ol>
  );
}
