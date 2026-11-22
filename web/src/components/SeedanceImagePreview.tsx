import { useEffect, useState } from "react";
import { Icon } from "@iconify/react";
import { buildSeedanceImageUrls, type SeedanceImage } from "@/lib/seedance";
import { useT } from "@/prefs/PrefsContext";

export function SeedanceImagePreview({
  images,
  runId,
  title,
}: {
  images: SeedanceImage[];
  runId?: string;
  title: string;
}) {
  const t = useT();
  const [failed, setFailed] = useState<Record<number, boolean>>({});
  return (
    <div className="grid gap-3 p-3 sm:grid-cols-2" data-testid="chat-seedance-images">
      {images.map((image) => (
        <SeedanceImagePreviewItem
          key={image.index}
          image={image}
          runId={runId}
          title={title}
          failed={failed[image.index] || false}
          onFailure={() => setFailed((prev) => ({ ...prev, [image.index]: true }))}
        />
      ))}
      {images.length === 0 ? (
        <div className="flex items-center gap-2 px-1 py-2 text-xs text-muted">
          <Icon icon="gravity-ui:picture" className="size-4 shrink-0" />
          <span>{t("seedance.previewUnavailable")}</span>
        </div>
      ) : null}
    </div>
  );
}

function SeedanceImagePreviewItem({
  image,
  runId,
  title,
  failed,
  onFailure,
}: {
  image: SeedanceImage;
  runId?: string;
  title: string;
  failed: boolean;
  onFailure: () => void;
}) {
  const t = useT();
  const sources = buildSeedanceImageUrls(image, runId);
  const [sourceIdx, setSourceIdx] = useState(0);
  const primarySource = sources[0] || "";
  useEffect(() => setSourceIdx(0), [primarySource]);
  const url = sources[sourceIdx];
  return (
    <figure className="m-0 overflow-hidden r-inset border border-border bg-surface-secondary">
      {url && !failed ? (
        <img
          src={url}
          alt={`${title} ${image.index}`}
          loading="lazy"
          className="max-h-[480px] w-full object-contain"
          onError={() => {
            if (sourceIdx + 1 < sources.length) setSourceIdx((idx) => idx + 1);
            else onFailure();
          }}
          data-testid="chat-seedance-image"
        />
      ) : (
        <div className="flex min-h-24 items-center justify-center gap-2 p-4 text-xs text-muted">
          <Icon icon="gravity-ui:picture" className="size-4 shrink-0" />
          <span>{t("seedance.previewUnavailable")}</span>
        </div>
      )}
      {url ? (
        <figcaption className="flex justify-end border-t border-border px-2 py-1.5">
          <a href={url} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 text-xs text-muted hover:text-foreground">
            <span>{t("seedance.openImage")}</span>
            <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-3" />
          </a>
        </figcaption>
      ) : null}
    </figure>
  );
}
