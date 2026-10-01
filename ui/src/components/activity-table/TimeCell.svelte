<script lang="ts">
  import { formatAbsoluteTime, formatRelativeTime } from "../../lib/format";
  import { locale } from "../../lib/i18n";

  interface Props {
    timestamp: string;
  }

  let { timestamp }: Props = $props();
  let absolute = $derived(formatAbsoluteTime(timestamp, $locale));
  let relative = $derived(formatRelativeTime(timestamp, $locale));
</script>

<span title={absolute} aria-label={absolute}>{relative}</span>
