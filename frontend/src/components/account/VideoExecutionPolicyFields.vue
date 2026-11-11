<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { defaultVideoExecutionPolicy, type VideoExecutionPolicy } from './videoExecutionPolicy'

const props = defineProps<{ modelValue: VideoExecutionPolicy | null; seedance?: boolean; protocol?: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: VideoExecutionPolicy | null] }>()
const { t } = useI18n()
const defaults = computed(() => defaultVideoExecutionPolicy(!!props.seedance, props.protocol || 'getoken'))
const policy = computed(() => ({ ...defaults.value, ...props.modelValue }))
const fields = [
  { key: 'request_timeout_seconds', label: 'requestTimeout', min: 5, max: 1200 },
  { key: 'task_timeout_seconds', label: 'taskTimeout', min: 60, max: 259200 },
  { key: 'max_retries', label: 'maxRetries', min: 0, max: 10 },
  { key: 'retry_interval_seconds', label: 'retryInterval', min: 5, max: 3600 },
] as const
function numberChanged(key: string, event: Event) {
  emit('update:modelValue', { ...policy.value, [key]: Number((event.target as HTMLInputElement).value) })
}
</script>

<template>
  <fieldset class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600" data-testid="video-execution-policy">
    <legend class="px-1 text-sm font-medium">{{ t('admin.accounts.videoExecution.title') }}</legend>
    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" :checked="modelValue !== null" data-testid="video-policy-custom" @change="emit('update:modelValue', ($event.target as HTMLInputElement).checked ? { ...defaults } : null)" />
      {{ t('admin.accounts.videoExecution.custom') }}
    </label>
    <p class="input-hint">{{ t('admin.accounts.videoExecution.hint') }}</p>
    <div class="grid grid-cols-2 gap-3">
      <label v-for="field in fields" :key="field.key" class="text-xs">
        <span class="mb-1 block">{{ t(`admin.accounts.videoExecution.${field.label}`) }}</span>
        <input type="number" step="1" :min="field.min" :max="field.max" :value="policy[field.key]" :disabled="!modelValue || (!policy.retry_enabled && (field.key === 'max_retries' || field.key === 'retry_interval_seconds'))" :aria-label="t(`admin.accounts.videoExecution.${field.label}`)" :data-testid="`video-policy-${field.key}`" class="input disabled:opacity-60" @input="numberChanged(field.key, $event)" />
      </label>
    </div>
    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" :disabled="!modelValue" :checked="policy.retry_enabled" data-testid="video-policy-retry" @change="emit('update:modelValue', { ...policy, retry_enabled: ($event.target as HTMLInputElement).checked })" />
      {{ t('admin.accounts.videoExecution.retry') }}
    </label>
    <p class="text-xs text-amber-700 dark:text-amber-300">{{ t('admin.accounts.videoExecution.safetyHint') }}</p>
  </fieldset>
</template>
