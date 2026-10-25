<script setup lang="ts">
import { computed } from 'vue'
import type { TemplateField } from '../api/types'

// 按模板 manifest 字段定义动态渲染录入控件；v-model 写入 fields 字典。
const props = defineProps<{
  fields: TemplateField[]
  modelValue: Record<string, unknown>
}>()
const emit = defineEmits<{ 'update:modelValue': [Record<string, unknown>] }>()

const editable = computed(() => props.fields.filter((f) => !f.readonly))

function get(f: TemplateField): unknown {
  return props.modelValue[f.field_id] ?? f.default ?? undefined
}
function set(f: TemplateField, v: unknown) {
  emit('update:modelValue', { ...props.modelValue, [f.field_id]: v })
}
</script>

<template>
  <el-form-item v-for="f in editable" :key="f.field_id" :label="f.label" :required="f.required">
    <el-input
      v-if="f.type === 'text'"
      :model-value="(get(f) as string) ?? ''"
      :maxlength="f.max_len || undefined"
      :placeholder="f.help"
      @update:model-value="set(f, $event)"
    />
    <el-input
      v-else-if="f.type === 'textarea'"
      type="textarea"
      :rows="3"
      :model-value="(get(f) as string) ?? ''"
      :maxlength="f.max_len || undefined"
      :placeholder="f.help"
      @update:model-value="set(f, $event)"
    />
    <el-input-number
      v-else-if="f.type === 'number' || f.type === 'money'"
      :model-value="(get(f) as number) ?? undefined"
      :controls="false"
      class="num"
      @update:model-value="set(f, $event)"
    />
    <el-date-picker
      v-else-if="f.type === 'date'"
      :model-value="(get(f) as string) ?? undefined"
      type="date"
      value-format="YYYY-MM-DD"
      @update:model-value="set(f, $event)"
    />
    <el-switch v-else-if="f.type === 'bool'" :model-value="Boolean(get(f))" @update:model-value="set(f, $event)" />
    <el-select
      v-else-if="f.type === 'enum'"
      :model-value="(get(f) as string) ?? undefined"
      clearable
      :placeholder="f.help || '请选择'"
      @update:model-value="set(f, $event)"
    >
      <el-option v-for="o in f.options ?? []" :key="o" :label="o" :value="o" />
    </el-select>
    <el-input v-else :model-value="(get(f) as string) ?? ''" @update:model-value="set(f, $event)" />
  </el-form-item>
</template>

<style scoped>
.num {
  width: 200px;
}
</style>
