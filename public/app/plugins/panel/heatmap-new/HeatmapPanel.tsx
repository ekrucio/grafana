import { css } from '@emotion/css';
import React, { useCallback, useMemo, useRef, useState } from 'react';

import { DataFrameType, GrafanaTheme2, PanelProps, reduceField, ReducerID, TimeRange } from '@grafana/data';
import { PanelDataErrorView } from '@grafana/runtime';
import { ScaleDistributionConfig } from '@grafana/schema';
import {
  Portal,
  ScaleDistribution,
  UPlotChart,
  useStyles2,
  useTheme2,
  VizLayout,
  VizTooltipContainer,
} from '@grafana/ui';
import { CloseButton } from 'app/core/components/CloseButton/CloseButton';
import { ColorScale } from 'app/core/components/ColorScale/ColorScale';
import { readHeatmapScanlinesCustomMeta } from 'app/features/transformers/calculateHeatmap/heatmap';

import { HeatmapHoverView } from './HeatmapHoverView';
import { prepareHeatmapData } from './fields';
import { PanelOptions } from './models.gen';
import { quantizeScheme } from './palettes';
import { HeatmapHoverEvent, prepConfig } from './utils';

interface HeatmapPanelProps extends PanelProps<PanelOptions> {}

export const HeatmapPanel: React.FC<HeatmapPanelProps> = ({
  data,
  id,
  timeRange,
  timeZone,
  width,
  height,
  options,
  fieldConfig,
  onChangeTimeRange,
  replaceVariables,
}) => {
  const theme = useTheme2();
  const styles = useStyles2(getStyles);

  // ugh
  let timeRangeRef = useRef<TimeRange>(timeRange);
  timeRangeRef.current = timeRange;

  const info = useMemo(() => {
    try {
      return prepareHeatmapData(data, options, theme);
    } catch (ex) {
      return { warning: `${ex}` };
    }
  }, [data, options, theme]);

  const facets = useMemo(() => {
    let exemplarsXFacet: number[] = []; // "Time" field
    let exemplarsyFacet: number[] = [];

    const meta = readHeatmapScanlinesCustomMeta(info.heatmap);
    if (info.exemplars?.length && meta.yMatchWithLabel) {
      exemplarsXFacet = info.exemplars?.fields[0].values.toArray();

      // ordinal/labeled heatmap-buckets?
      const hasLabeledY = meta.yOrdinalDisplay != null;

      if (hasLabeledY) {
        let matchExemplarsBy = info.exemplars?.fields
          .find((field) => field.name === meta.yMatchWithLabel)!
          .values.toArray();
        exemplarsyFacet = matchExemplarsBy.map((label) => meta.yOrdinalLabel?.indexOf(label)) as number[];
      } else {
        exemplarsyFacet = info.exemplars?.fields[1].values.toArray() as number[]; // "Value" field
      }
    }

    return [null, info.heatmap?.fields.map((f) => f.values.toArray()), [exemplarsXFacet, exemplarsyFacet]];
  }, [info.heatmap, info.exemplars]);

  const palette = useMemo(() => quantizeScheme(options.color, theme), [options.color, theme]);

  const [hover, setHover] = useState<HeatmapHoverEvent | undefined>(undefined);
  const [shouldDisplayCloseButton, setShouldDisplayCloseButton] = useState<boolean>(false);
  const isToolTipOpen = useRef<boolean>(false);

  const onCloseToolTip = () => {
    isToolTipOpen.current = false;
    setShouldDisplayCloseButton(false);
    onhover(null);
  };

  const onclick = () => {
    isToolTipOpen.current = !isToolTipOpen.current;

    // Linking into useState required to re-render tooltip
    setShouldDisplayCloseButton(isToolTipOpen.current);
  };

  const onhover = useCallback(
    (evt?: HeatmapHoverEvent | null) => {
      setHover(evt ?? undefined);
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [options, data.structureRev]
  );

  // ugh
  const dataRef = useRef(info);
  dataRef.current = info;

  const builder = useMemo(() => {
    const scaleConfig = dataRef.current?.heatmap?.fields[1].config?.custom
      ?.scaleDistribution as ScaleDistributionConfig;
    return prepConfig({
      dataRef,
      theme,
      onhover: onhover,
      onclick: options.tooltip.show ? onclick : null,
      onzoom: (evt) => {
        const delta = evt.xMax - evt.xMin;
        if (delta > 1) {
          onChangeTimeRange({ from: evt.xMin, to: evt.xMax });
        }
      },
      isToolTipOpen,
      timeZone,
      getTimeRange: () => timeRangeRef.current,
      palette,
      cellGap: options.cellGap,
      hideLE: options.filterValues?.le,
      hideGE: options.filterValues?.ge,
      valueMin: options.color.min,
      valueMax: options.color.max,
      exemplarColor: options.exemplars?.color ?? 'rgba(255,0,255,0.7)',
      yAxisConfig: options.yAxis,
      ySizeDivisor: scaleConfig?.type === ScaleDistribution.Log ? +(options.calculation?.yBuckets?.value || 1) : 1,
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [options, data.structureRev]);

  const renderLegend = () => {
    if (!info.heatmap || !options.legend.show) {
      return null;
    }

    let heatmapType = dataRef.current?.heatmap?.meta?.type;
    let countFieldIdx = heatmapType === DataFrameType.HeatmapScanlines ? 2 : 3;
    const countField = info.heatmap.fields[countFieldIdx];

    // TODO -- better would be to get the range from the real color scale!
    let { min, max } = options.color;
    if (min == null || max == null) {
      const calc = reduceField({ field: countField, reducers: [ReducerID.min, ReducerID.max] });
      if (min == null) {
        min = calc[ReducerID.min];
      }
      if (max == null) {
        max = calc[ReducerID.max];
      }
    }

    let hoverValue: number | undefined = undefined;
    // seriesIdx: 1 is heatmap layer; 2 is exemplar layer
    if (hover && info.heatmap.fields && hover.seriesIdx === 1) {
      hoverValue = countField.values.get(hover.dataIdx);
    }

    return (
      <VizLayout.Legend placement="bottom" maxHeight="20%">
        <div className={styles.colorScaleWrapper}>
          <ColorScale hoverValue={hoverValue} colorPalette={palette} min={min!} max={max!} display={info.display} />
        </div>
      </VizLayout.Legend>
    );
  };

  if (info.warning || !info.heatmap) {
    return (
      <PanelDataErrorView
        panelId={id}
        fieldConfig={fieldConfig}
        data={data}
        needsNumberField={true}
        message={info.warning}
      />
    );
  }

  return (
    <>
      <VizLayout width={width} height={height} legend={renderLegend()}>
        {(vizWidth: number, vizHeight: number) => (
          <UPlotChart config={builder} data={facets as any} width={vizWidth} height={vizHeight} timeRange={timeRange}>
            {/*children ? children(config, alignedFrame) : null*/}
          </UPlotChart>
        )}
      </VizLayout>
      <Portal>
        {hover && options.tooltip.show && (
          <VizTooltipContainer
            position={{ x: hover.pageX, y: hover.pageY }}
            offset={{ x: 10, y: 10 }}
            allowPointerEvents={isToolTipOpen.current}
          >
            {shouldDisplayCloseButton && (
              <>
                <CloseButton onClick={onCloseToolTip} />
                <div className={styles.closeButtonSpacer} />
              </>
            )}
            <HeatmapHoverView data={info} hover={hover} showHistogram={options.tooltip.yHistogram} />
          </VizTooltipContainer>
        )}
      </Portal>
    </>
  );
};

const getStyles = (theme: GrafanaTheme2) => ({
  closeButtonSpacer: css`
    margin-bottom: 15px;
  `,
  colorScaleWrapper: css`
    margin-left: 25px;
    padding: 10px 0;
    max-width: 300px;
  `,
});
