import { WrenchScrewdriverIcon } from "@heroicons/react/24/outline";
import { PageHeader } from "./PageHeader";

/**
 * Styled stand-in for pages a follow-up agent will build in phase 2. Keeps the
 * route registered and navigable with a consistent shell.
 */
export function Placeholder({ title, description }: { title: string; description?: string }) {
  return (
    <div>
      <PageHeader title={title} description={description} />
      <div className="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-zinc-800 bg-zinc-900/30 px-6 py-20 text-center">
        <div className="rounded-full border border-zinc-800 bg-zinc-900 p-3">
          <WrenchScrewdriverIcon className="h-6 w-6 text-zinc-600" />
        </div>
        <h2 className="text-sm font-medium text-zinc-300">{title} is coming in phase 2</h2>
        <p className="max-w-sm text-xs leading-relaxed text-zinc-500">
          This screen is registered and reachable. A follow-up pass will wire it to its service
          methods and flesh out the full experience.
        </p>
      </div>
    </div>
  );
}
